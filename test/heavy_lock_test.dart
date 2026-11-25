import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:test/test.dart';

import 'package:heavy_lock/heavy_lock.dart';
import 'package:heavy_lock/src/heavy_lock_status.dart';

void main() {
  final runner = File('bin/heavy_lock.dart').absolute.path;
  final dart = Platform.resolvedExecutable;
  late Directory temporary;
  late String gate;
  late File job;

  setUp(() async {
    temporary = await Directory.systemTemp.createTemp('heavy_lock_test');
    gate = '${temporary.path}${Platform.pathSeparator}heavy-lock.lock';
    // Records "<name> start|end <epoch ms>" lines. In between it sleeps for
    // a number of milliseconds or, given "@<path>", waits until <path>
    // exists, so tests can order events without relying on timing.
    job = File('${temporary.path}${Platform.pathSeparator}job.dart');
    await job.writeAsString('''
import 'dart:io';
Future<void> main(List<String> a) async {
  final log = File(a[0]);
  log.writeAsStringSync('\${a[1]} start \${DateTime.now().millisecondsSinceEpoch}\\n', mode: FileMode.append, flush: true);
  if (a[2].startsWith('@')) {
    while (!File(a[2].substring(1)).existsSync()) {
      await Future<void>.delayed(const Duration(milliseconds: 20));
    }
  } else {
    await Future<void>.delayed(Duration(milliseconds: int.parse(a[2])));
  }
  log.writeAsStringSync('\${a[1]} end \${DateTime.now().millisecondsSinceEpoch}\\n', mode: FileMode.append, flush: true);
  exit(a.length > 3 ? int.parse(a[3]) : 0);
}
''');
  });

  tearDown(() => temporary.delete(recursive: true));

  Map<String, String> environment({
    String capacity = '2',
    String maxCpu = '1000000',
  }) => {
    ...Platform.environment,
    lockPathVariable: gate,
    heldLockVariable: '',
    capacityVariable: capacity,
    minFreeMemoryVariable: '0',
    maxCpuLoadVariable: maxCpu,
    settleVariable: '0',
    priorityVariable: 'normal',
  }..remove('HEAVY_LOCK_WORKLOG');

  final errors = Expando<StringBuffer>();

  Future<Process> started(Future<Process> starting) async {
    final process = await starting;
    final buffer = errors[process] = StringBuffer();
    process.stderr.transform(utf8.decoder).listen(buffer.write);
    unawaited(process.stdout.drain<void>());
    return process;
  }

  Future<Process> start(
    List<String> options,
    List<String> command, {
    Map<String, String>? env,
  }) => started(
    Process.start(
      dart,
      [runner, ...options, '--', ...command],
      environment: env ?? environment(),
      includeParentEnvironment: false,
    ),
  );

  List<String> sleeper(String name, int milliseconds, [int code = 0]) => [
    dart,
    job.path,
    '${temporary.path}${Platform.pathSeparator}log.txt',
    name,
    '$milliseconds',
    '$code',
  ];

  String go(String name) =>
      '${temporary.path}${Platform.pathSeparator}$name.go';

  // A job that runs until [release] is called with the same name.
  List<String> holder(String name) => [
    dart,
    job.path,
    '${temporary.path}${Platform.pathSeparator}log.txt',
    name,
    '@${go(name)}',
  ];

  void release(String name) => File(go(name)).writeAsStringSync('');

  Future<Map<String, (int, int)>> intervals() async {
    final times = <String, Map<String, int>>{};
    for (final line in await File(
      '${temporary.path}${Platform.pathSeparator}log.txt',
    ).readAsLines()) {
      final [name, event, at] = line.split(' ');
      (times[name] ??= {})[event] = int.parse(at);
    }
    return {
      for (final MapEntry(:key, :value) in times.entries)
        key: (value['start']!, value['end']!),
    };
  }

  bool overlap((int, int) a, (int, int) b) => a.$1 < b.$2 && b.$1 < a.$2;

  Future<void> waitForError(Process process, String text) async {
    while (!errors[process]!.toString().contains(text)) {
      await Future<void>.delayed(const Duration(milliseconds: 20));
    }
  }

  Future<(int, String)> finish(Process process) async {
    final code = await process.exitCode;
    // Lets the stderr listener deliver the last chunk.
    await Future<void>.delayed(const Duration(milliseconds: 50));
    return (code, errors[process]!.toString());
  }

  Future<void> waitForLog(String name) async {
    final log = File('${temporary.path}${Platform.pathSeparator}log.txt');
    while (!(await log.exists()) ||
        !(await log.readAsString()).contains('$name start')) {
      await Future<void>.delayed(const Duration(milliseconds: 50));
    }
  }

  Future<List<Map<String, dynamic>>> statuses(
    int count, {
    String? gatePath,
  }) async {
    final path = gatePath ?? gate;
    final stem = path.endsWith('.lock')
        ? path.substring(0, path.length - 5)
        : path;
    final directory = Directory('$stem.status');
    final deadline = DateTime.now().add(const Duration(seconds: 10));
    while (DateTime.now().isBefore(deadline)) {
      if (await directory.exists()) {
        final files = (await directory.list().toList()).whereType<File>().where(
          (file) => file.path.endsWith('.json'),
        );
        final records = <Map<String, dynamic>>[];
        for (final file in files) {
          records.add(
            jsonDecode(await file.readAsString()) as Map<String, dynamic>,
          );
        }
        if (records.length == count) return records;
      }
      await Future<void>.delayed(const Duration(milliseconds: 20));
    }
    throw StateError('Expected $count status records');
  }

  group('parsing', () {
    test(
      'reads a Linux process start even when its name contains parentheses',
      () {
        expect(
          parseLinuxProcessStart(
            '123 (hello (world)) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 12345 0',
          ),
          '12345',
        );
        expect(parseLinuxProcessStart('123 (dead) Z 1 2'), isNull);
      },
    );
    test('reads macOS memory and load', () {
      final sample = parseDarwinSample(
        '8589934592\n{ 3.00 2.00 1.00 }\n36\n',
        processors: 6,
      )!;
      expect(sample.totalMemoryBytes, 8589934592);
      expect(sample.freeMemoryPercent, closeTo(36, 0.01));
      expect(sample.cpuLoadPercent, 50);
    });

    test('reads Linux and Windows samples', () {
      final linux = parseLinuxSample(
        'MemTotal:       16000000 kB\nMemAvailable:    4000000 kB\n',
        '1.50 1.00 0.50 2/300 1234\n',
        processors: 4,
      )!;
      expect(linux.freeMemoryPercent, 25);
      expect(linux.cpuLoadPercent, 37.5);
      final windows = parseWindowsSample('16000000 8000000 12\r\n')!;
      expect(windows.freeMemoryPercent, 50);
      expect(windows.cpuLoadPercent, 12);
    });

    test('rejects malformed samples', () {
      expect(parseDarwinSample('', processors: 1), isNull);
      expect(
        parseDarwinSample('8589934592\n{ 1.00 1.00 1.00 }\n', processors: 1),
        isNull,
      );
      expect(
        parseDarwinSample('8589934592\n{ 1.00 1.00 1.00 }\n101', processors: 1),
        isNull,
      );
      expect(parseLinuxSample('MemTotal: 1 kB', '', processors: 1), isNull);
      expect(parseWindowsSample('0 0 0'), isNull);
      expect(parseWindowsSample('1 2'), isNull);
    });

    test('parses weights and priorities', () {
      expect(parseWeight('heavy'), heavyWeight);
      expect(parseWeight('light'), lightWeight);
      expect(parseWeight('3'), 3);
      expect(parseWeight('0'), isNull);
      expect(HeavyPriority.parse(null), HeavyPriority.low);
      expect(HeavyPriority.parse('background'), HeavyPriority.background);
      expect(HeavyPriority.parse('idle'), isNull);
    });
  });

  group('policy', () {
    test('a modest machine fits one heavy job', () {
      int capacity(int processors, int? gib) => defaultCapacity(
        processors: processors,
        totalMemoryBytes: gib == null ? null : gib << 30,
      );
      expect(capacity(6, 8), heavyWeight);
      expect(capacity(32, 8), heavyWeight);
      expect(capacity(6, null), heavyWeight);
      expect(capacity(16, 64), 5);
      expect(capacity(64, 256), 8);
    });

    test('sharing requires settled admissions and free resources', () {
      const config = HeavyLockConfig(
        gatePath: 'x.lock',
        capacity: 4,
        minFreeMemoryPercent: 25,
        maxCpuLoadPercent: 50,
        settle: Duration(seconds: 60),
      );
      ResourceSample sample(int freeGiB, double cpu) => ResourceSample(
        totalMemoryBytes: 8 << 30,
        availableMemoryBytes: freeGiB << 30,
        cpuLoadPercent: cpu,
      );
      String? refusal(ResourceSample? value, [int seconds = 120]) =>
          sharingRefusal(
            config: config,
            sample: value,
            sinceLastAdmission: Duration(seconds: seconds),
          );
      expect(refusal(sample(4, 20)), isNull);
      expect(refusal(sample(4, 20), 10), contains('settle'));
      expect(refusal(null), contains('alone'));
      expect(refusal(sample(1, 20)), contains('free resources'));
      expect(refusal(sample(4, 80)), contains('free resources'));
      expect(config.slotPath(1), 'x.slot-1.lock');
      expect(config.admissionPath, 'x.admission.lock');
    });

    test('maps priorities to taskpolicy or nice', () {
      if (Platform.isWindows) {
        expect(
          posixPriorityPrefix(HeavyPriority.background, hasTaskpolicy: true),
          isEmpty,
        );
        return;
      }
      expect(
        posixPriorityPrefix(HeavyPriority.background, hasTaskpolicy: true),
        ['/usr/sbin/taskpolicy', '-b'],
      );
      expect(
        posixPriorityPrefix(HeavyPriority.background, hasTaskpolicy: false),
        ['nice', '-n', '10'],
      );
      expect(posixPriorityPrefix(HeavyPriority.low, hasTaskpolicy: true), [
        'nice',
        '-n',
        '10',
      ]);
      expect(
        posixPriorityPrefix(HeavyPriority.normal, hasTaskpolicy: true),
        isEmpty,
      );
    });
  });

  group('process tree', () {
    test('parses ps rows and drops zombies', () {
      expect(
        parseProcessTable(
          '  10     1 Ss   Mon Nov 24 01:02:03 2026\n'
          '  11    10 T+   Mon Nov 24 01:02:04 2026\n'
          '  12    10 Z    Mon Nov 24 01:02:05 2026\n',
        ),
        {
          10: (parent: 1, state: 'Ss', started: 'Mon Nov 24 01:02:03 2026'),
          11: (parent: 10, state: 'T+', started: 'Mon Nov 24 01:02:04 2026'),
        },
      );
    });

    test('a child forked after the snapshot is signalled before its parent '
        'can exit', () async {
      final kernel = _FakeKernel();
      final root = kernel.spawn(1);
      final parent = kernel.spawn(root);
      final tree = kernel.tree(root);
      await tree.refresh();
      int? child;
      // The fork lands between the snapshot and the signal: the window in
      // which a snapshot-then-signal runner orphans the child.
      kernel.afterSnapshot = () {
        if (child == null && !kernel.isStopped(parent)) {
          child = kernel.spawn(parent);
        }
      };
      expect(await tree.signal(ProcessSignal.sigterm), isTrue);
      expect(child, isNotNull);
      expect(kernel.alive, {1}, reason: 'no process of the tree survives');
      expect(
        kernel.log.indexOf('$child SIGSTOP'),
        lessThan(kernel.log.indexOf('$parent SIGTERM')),
        reason: 'the child is found and stopped before the parent is told',
      );
    });

    test('waits for a second snapshot when ps is not atomic', () async {
      final kernel = _FakeKernel();
      final root = kernel.spawn(1);
      final parent = kernel.spawn(root);
      final tree = kernel.tree(root);
      await tree.refresh();
      int? child;
      // The parent forks just before its stop takes effect, while `ps` has
      // already listed the processes: that snapshot shows every tracked
      // process stopped but lacks the child.
      kernel.beforeStop = (pid) {
        if (pid == parent && child == null) {
          child = kernel.spawn(parent);
        }
      };
      expect(await tree.signal(ProcessSignal.sigterm), isTrue);
      expect(kernel.alive, {1});
      expect(kernel.log, contains('$child SIGTERM'));
    });

    test('continues the processes it stopped and signals only new ones '
        'with onlyNew', () async {
      final kernel = _FakeKernel();
      final root = kernel.spawn(1, ignoresTerm: true);
      final tree = kernel.tree(root);
      await tree.refresh();
      await tree.signal(ProcessSignal.sigterm);
      expect(kernel.alive, {1, root});
      expect(kernel.isStopped(root), isFalse);
      expect(tree.hasUnsignalled, isFalse);
      final late = kernel.spawn(root, ignoresTerm: true);
      await tree.refresh();
      expect(tree.hasUnsignalled, isTrue);
      kernel.log.clear();
      await tree.signal(ProcessSignal.sigterm, onlyNew: true);
      expect(kernel.log.where((entry) => entry.endsWith('SIGTERM')), [
        '$late SIGTERM',
      ]);
      expect(kernel.isStopped(root) || kernel.isStopped(late), isFalse);
      await tree.signal(ProcessSignal.sigkill);
      expect(kernel.alive, {1});
    });

    test('continues descendants before the parents that may exit', () async {
      final kernel = _FakeKernel();
      final root = kernel.spawn(1);
      final tree = kernel.tree(root);
      await tree.refresh();
      int? child;
      kernel.afterSnapshot = () {
        child ??= kernel.spawn(root, ignoresTerm: true);
      };
      await tree.signal(ProcessSignal.sigterm);
      expect(kernel.log, isNot(contains('$child SIGHUP')));
      expect(kernel.alive, {1, child});
      final continued = [
        for (final entry in kernel.log)
          if (entry.endsWith('SIGCONT')) entry,
      ];
      expect(continued, ['$child SIGCONT', '$root SIGCONT']);
    });

    test('signals anyway when the tree does not stop in time', () async {
      final kernel = _FakeKernel();
      final root = kernel.spawn(1, stoppable: false);
      final tree = kernel.tree(
        root,
        freezeLimit: const Duration(milliseconds: 50),
      );
      await tree.refresh();
      expect(await tree.signal(ProcessSignal.sigterm), isFalse);
      expect(kernel.alive, {1});
    });
  });

  group('runner', () {
    test('uses the neutral default gate and nesting marker', () async {
      final probe = File(
        '${temporary.path}${Platform.pathSeparator}probe.dart',
      );
      final marker = File('${temporary.path}${Platform.pathSeparator}marker');
      await probe.writeAsString('''
import 'dart:io';
void main(List<String> arguments) {
  File(arguments.single).writeAsStringSync(
    Platform.environment['HEAVY_LOCK_HELD'] ?? '',
  );
}
''');
      final env = environment()
        ..['HOME'] = temporary.path
        ..['LOCALAPPDATA'] = temporary.path
        ..['HEAVY_LOCK_CAPACITY'] = '2'
        ..['HEAVY_LOCK_PRIORITY'] = 'normal'
        ..remove('HEAVY_LOCK_PATH')
        ..remove('HEAVY_LOCK_HELD');
      final expectedGate = Platform.isWindows
          ? '${temporary.path}\\heavy-lock\\heavy-lock.lock'
          : '${temporary.path}/.cache/heavy-lock.lock';
      final result = await Process.run(
        dart,
        [runner, '--', dart, probe.path, marker.path],
        environment: env,
        includeParentEnvironment: false,
      );
      expect(result.exitCode, 0, reason: '${result.stderr}');
      expect(await File(expectedGate).exists(), isTrue);
      expect(await marker.readAsString(), File(expectedGate).absolute.path);

      final status = await Process.run(
        dart,
        ['bin/heavy_lock_status.dart'],
        environment: env,
        includeParentEnvironment: false,
      );
      expect(status.exitCode, 0, reason: '${status.stderr}');
      expect(
        status.stdout,
        contains('Gate: ${File(expectedGate).absolute.path}'),
      );
    });

    test('honors configured path, capacity and worklog environment', () async {
      final configuredGate =
          '${temporary.path}${Platform.pathSeparator}configured.lock';
      final env = {
        ...environment(),
        'HEAVY_LOCK_PATH': configuredGate,
        'HEAVY_LOCK_CAPACITY': '1',
        'HEAVY_LOCK_WORKLOG': '123',
        'HEAVY_LOCK_PRIORITY': 'normal',
      };
      final process = await start([], holder('configured'), env: env);
      try {
        await waitForLog('configured');
        expect(await File(configuredGate).exists(), isTrue);
        final record = (await statuses(1, gatePath: configuredGate)).single;
        expect(record['capacity'], 1);
        expect(record['worklog'], 123);
        final status = await Process.run(
          dart,
          ['bin/heavy_lock_status.dart', '--json'],
          environment: env,
          includeParentEnvironment: false,
        );
        expect(status.exitCode, 0, reason: '${status.stderr}');
        expect(
          (jsonDecode(status.stdout as String) as List<dynamic>).single['pid'],
          process.pid,
        );
      } finally {
        release('configured');
        await finish(process);
      }
    });

    test('validates neutral worklog and priority settings', () async {
      for (final (name, value) in [
        ('HEAVY_LOCK_WORKLOG', '0'),
        ('HEAVY_LOCK_PRIORITY', 'invalid'),
      ]) {
        final (code, error) = await finish(
          await start(
            [],
            sleeper(name, 0),
            env: {...environment(), name: value},
          ),
        );
        expect(code, usageExitCode, reason: name);
        expect(error, contains(name));
      }
    });

    test('honors neutral sharing thresholds and settle time', () async {
      for (final (name, value) in [
        ('HEAVY_LOCK_MIN_FREE_MEMORY_PERCENT', '101'),
        ('HEAVY_LOCK_MAX_CPU_LOAD_PERCENT', '-1'),
        ('HEAVY_LOCK_SETTLE_SECONDS', '60'),
      ]) {
        final env = {
          ...environment(),
          'HEAVY_LOCK_PATH': gate,
          'HEAVY_LOCK_CAPACITY': '2',
          'HEAVY_LOCK_MIN_FREE_MEMORY_PERCENT': '0',
          'HEAVY_LOCK_MAX_CPU_LOAD_PERCENT': '1000000',
          'HEAVY_LOCK_SETTLE_SECONDS': '0',
          'HEAVY_LOCK_PRIORITY': 'normal',
          name: value,
        };
        final first = await start(
          ['--weight', 'light'],
          holder(name),
          env: env,
        );
        try {
          await waitForLog(name);
          final (control, controlError) = await finish(
            await start(
              ['--no-wait', '--weight', 'light'],
              sleeper('control', 0),
              env: {
                ...env,
                'HEAVY_LOCK_MIN_FREE_MEMORY_PERCENT': '0',
                'HEAVY_LOCK_MAX_CPU_LOAD_PERCENT': '1000000',
                'HEAVY_LOCK_SETTLE_SECONDS': '0',
              },
            ),
          );
          expect(control, 0, reason: 'control for $name: $controlError');
          final (code, _) = await finish(
            await start(
              ['--no-wait', '--weight', 'light'],
              sleeper('second', 0),
              env: env,
            ),
          );
          expect(code, busyExitCode, reason: name);
        } finally {
          release(name);
          await finish(first);
        }
      }
    });

    test('help succeeds without starting a command', () async {
      final result = await Process.run(dart, [
        runner,
        '--help',
      ], environment: environment());
      expect(result.exitCode, 0);
      expect(result.stdout, contains('Usage: heavy-lock'));
      expect(await File(gate).exists(), isFalse);
    });

    test('status CLI reads the configured directory as JSON', () async {
      final process = await start([], holder('status-cli'));
      addTearDown(() => process.kill());
      await waitForLog('status-cli');
      await statuses(1);
      final result = await Process.run(dart, [
        'bin/heavy_lock_status.dart',
        '--json',
      ], environment: environment());
      expect(result.exitCode, 0);
      final records = jsonDecode(result.stdout as String) as List<dynamic>;
      expect(records, hasLength(1));
      expect(records.single['pid'], process.pid);
      expect(records.single['command'], holder('status-cli'));
      release('status-cli');
      expect((await finish(process)).$1, 0);
    });

    test('status CLI distinguishes absent and malformed records', () async {
      Future<ProcessResult> status() => Process.run(dart, [
        'bin/heavy_lock_status.dart',
        '--json',
      ], environment: environment());
      final empty = await status();
      expect(empty.exitCode, 0);
      expect(jsonDecode(empty.stdout as String), isEmpty);
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.create();
      await File('${directory.path}${Platform.pathSeparator}broken.json')
          .writeAsString('{broken');
      final broken = await status();
      expect(broken.exitCode, softwareExitCode);
      expect(broken.stderr, contains('heavy-lock-status: failed:'));
      expect(broken.stdout, isEmpty);
    });

    test('prunes a stale record for a reused pid', () async {
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.create();
      final stale = File(
        '${directory.path}${Platform.pathSeparator}stale.json',
      );
      await stale.writeAsString(
        jsonEncode({'version': 1, 'pid': pid, 'processStart': '1:1'}),
      );
      final process = await start([], holder('prune'));
      addTearDown(() => process.kill());
      await waitForLog('prune');
      final deadline = DateTime.now().add(const Duration(seconds: 5));
      while (await stale.exists() && DateTime.now().isBefore(deadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await stale.exists(), isFalse);
      release('prune');
      expect((await finish(process)).$1, 0);
    }, skip: !Platform.isMacOS);

    test('prunes a stale record for a dead pid', () async {
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.create();
      final stale = File('${directory.path}${Platform.pathSeparator}dead.json');
      await stale.writeAsString(
        jsonEncode({'version': 1, 'pid': 2147483647, 'processStart': '1:1'}),
      );
      final process = await start([], holder('dead-record'));
      addTearDown(() => process.kill());
      await waitForLog('dead-record');
      final deadline = DateTime.now().add(const Duration(seconds: 5));
      while (await stale.exists() && DateTime.now().isBefore(deadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await stale.exists(), isFalse);
      release('dead-record');
      expect((await finish(process)).$1, 0);
    }, skip: Platform.isWindows);

    test('reports a waiting run and prunes it after SIGTERM', () async {
      final holderProcess = await start([], holder('holding'));
      addTearDown(() => holderProcess.kill());
      await waitForLog('holding');
      final waiting = await start(
        [],
        holder('waiting'),
        env: {...environment(), 'HEAVY_LOCK_WORKLOG': '234'},
      );
      addTearDown(() => waiting.kill());
      await waitForError(waiting, 'waiting for');
      var records = await statuses(2);
      var record = records.singleWhere(
        (record) => record['pid'] == waiting.pid,
      );
      final deadline = DateTime.now().add(const Duration(seconds: 5));
      while (record['reason'] == null && DateTime.now().isBefore(deadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
        records = await statuses(2);
        record = records.singleWhere((record) => record['pid'] == waiting.pid);
      }
      expect(record['state'], 'waiting');
      expect(record['worklog'], 234);
      expect(record['reason'], contains('waiting for'));
      waiting.kill(ProcessSignal.sigterm);
      expect((await finish(waiting)).$1, 143);
      release('holding');
      expect((await finish(holderProcess)).$1, 0);
      final next = await start([], holder('after-signal'));
      addTearDown(() => next.kill());
      await waitForLog('after-signal');
      expect((await statuses(1)).single['pid'], next.pid);
      release('after-signal');
      expect((await finish(next)).$1, 0);
    }, skip: Platform.isWindows);

    test('waiting SIGTERM does not wait for stalled status I/O', () async {
      final slot =
          '${temporary.path}${Platform.pathSeparator}heavy-lock.slot-0.lock';
      final blocker = await Process.start('/usr/bin/python3', [
        '-u',
        '-c',
        'import fcntl,sys; f=open(sys.argv[1],"w"); '
            'fcntl.lockf(f,fcntl.LOCK_EX); print("ready",flush=True); '
            'sys.stdin.read()',
        slot,
      ]);
      addTearDown(() async {
        await blocker.stdin.close();
        await blocker.exitCode;
      });
      expect(
        await blocker.stdout.transform(utf8.decoder).first,
        contains('ready'),
      );
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.create();
      expect(
        (await Process.run('/bin/chmod', ['700', directory.path])).exitCode,
        0,
      );
      final bin = Directory('${temporary.path}/bin');
      await bin.create();
      final entered = File('${temporary.path}/chmod-entered');
      final proceed = File('${temporary.path}/chmod-proceed');
      final finished = File('${temporary.path}/chmod-finished');
      final chmod = File('${bin.path}/chmod');
      await chmod.writeAsString('''
#!/bin/sh
touch "${entered.path}"
i=0
while [ ! -f "${proceed.path}" ] && [ "\$i" -lt 200 ]; do
  sleep 0.05
  i=\$((i + 1))
done
/bin/chmod "\$@"
touch "${finished.path}"
''');
      expect(
        (await Process.run('/bin/chmod', ['755', chmod.path])).exitCode,
        0,
      );
      final waiting = await start(
        [],
        sleeper('never-started', 0),
        env: {
          ...environment(),
          'PATH': '${bin.path}:${Platform.environment['PATH']}',
        },
      );
      addTearDown(() {
        proceed.writeAsStringSync('');
        waiting.kill();
      });
      await waitForError(waiting, 'waiting for');
      await statuses(1);
      expect(
        (await Process.run('/bin/chmod', ['755', directory.path])).exitCode,
        0,
      );
      final deadline = DateTime.now().add(const Duration(seconds: 10));
      while (!await entered.exists() && DateTime.now().isBefore(deadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await entered.exists(), isTrue);
      waiting.kill(ProcessSignal.sigterm);
      int? code;
      try {
        code = await waiting.exitCode.timeout(
          const Duration(milliseconds: 700),
        );
      } on TimeoutException {
        // Release the test helper before reporting a blocked signal path.
      } finally {
        await proceed.writeAsString('');
      }
      final finishDeadline = DateTime.now().add(const Duration(seconds: 5));
      while (!await finished.exists() &&
          DateTime.now().isBefore(finishDeadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await finished.exists(), isTrue);
      expect(
        code,
        143,
        reason: 'status I/O must not delay releasing the slots',
      );
      final released = await File(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.slot-1.lock',
      ).open(mode: FileMode.append);
      try {
        await released.lock(FileLock.exclusive);
      } finally {
        await released.close();
      }
    }, skip: Platform.isWindows);

    test('unwritable status location does not affect command exit', () async {
      final statusPath = File(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await statusPath.writeAsString('not a directory');
      final process = await start([], sleeper('write-failure', 100, 19));
      final result = await finish(process);
      expect(result.$1, 19);
      expect(result.$2, contains('status unavailable'));
    }, skip: Platform.isWindows);

    test('recreated status directory remains private', () async {
      final process = await started(
        Process.start(
          '/bin/sh',
          [
            '-c',
            'umask 022; exec "\$@"',
            'sh',
            dart,
            runner,
            '--',
            ...holder('recreated-status'),
          ],
          environment: environment(),
          includeParentEnvironment: false,
        ),
      );
      addTearDown(() => process.kill());
      await waitForLog('recreated-status');
      await statuses(1);
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.delete(recursive: true);
      await statuses(1);
      expect((await directory.stat()).mode & 0x1ff, 0x1c0);
      release('recreated-status');
      expect((await finish(process)).$1, 0);
    }, skip: Platform.isWindows);

    test('removes its record when a pending status write times out', () async {
      final bin = Directory('${temporary.path}/bin');
      await bin.create();
      final entered = File('${temporary.path}/chmod-entered');
      final proceed = File('${temporary.path}/chmod-proceed');
      final finished = File('${temporary.path}/chmod-finished');
      final chmod = File('${bin.path}/chmod');
      await chmod.writeAsString('''
#!/bin/sh
touch "${entered.path}"
i=0
while [ ! -f "${proceed.path}" ] && [ "\$i" -lt 200 ]; do
  sleep 0.05
  i=\$((i + 1))
done
/bin/chmod "\$@"
touch "${finished.path}"
''');
      expect(
        (await Process.run('/bin/chmod', ['755', chmod.path])).exitCode,
        0,
      );
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      await directory.create();
      expect(
        (await Process.run('/bin/chmod', ['700', directory.path])).exitCode,
        0,
      );
      final process = await start(
        [],
        holder('slow-status'),
        env: {
          ...environment(),
          'PATH':
              '${bin.path}${Platform.isWindows ? ';' : ':'}${Platform.environment['PATH']}',
        },
      );
      addTearDown(() {
        proceed.writeAsStringSync('');
        process.kill();
      });
      await waitForLog('slow-status');
      await statuses(1);
      expect(
        (await Process.run('/bin/chmod', ['755', directory.path])).exitCode,
        0,
      );
      final deadline = DateTime.now().add(const Duration(seconds: 10));
      while (!await entered.exists() && DateTime.now().isBefore(deadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await entered.exists(), isTrue);
      release('slow-status');
      expect((await finish(process)).$1, 0);
      final records = (await directory.list().toList())
          .where((file) => file.path.endsWith('.json'))
          .toList();
      await proceed.writeAsString('');
      final finishDeadline = DateTime.now().add(const Duration(seconds: 5));
      while (!await finished.exists() &&
          DateTime.now().isBefore(finishDeadline)) {
        await Future<void>.delayed(const Duration(milliseconds: 20));
      }
      expect(await finished.exists(), isTrue);
      expect(records, isEmpty);
    }, skip: Platform.isWindows);

    test('nested runner publishes only the parent unknown record', () async {
      final process = await start([], [
        dart,
        runner,
        '--',
        ...holder('nested-status'),
      ]);
      addTearDown(() => process.kill());
      await waitForLog('nested-status');
      final records = await statuses(1);
      expect(records.single['pid'], process.pid);
      expect(records.single['attribution'], 'unknown');
      expect(records.single['worklog'], isNull);
      release('nested-status');
      expect((await finish(process)).$1, 0);
    }, skip: Platform.isWindows);

    test('removes its status after a failing command', () async {
      final process = await start([], [...holder('failed-job'), '19']);
      addTearDown(() => process.kill());
      await waitForLog('failed-job');
      expect(await statuses(1), hasLength(1));
      release('failed-job');
      expect((await finish(process)).$1, 19);
      final directory = Directory(
        '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
      );
      final files = await directory.list().toList();
      expect(files.where((file) => file.path.endsWith('.json')), isEmpty);
    }, skip: Platform.isWindows);

    test('invalid declared worklog is rejected before running', () async {
      final result = await finish(
        await start(['--worklog', '0'], sleeper('bad', 0)),
      );
      expect(result.$1, usageExitCode);
      expect(
        await File('${temporary.path}${Platform.pathSeparator}log.txt')
            .exists(),
        isFalse,
      );
    });

    test(
      'publishes the declared worklog while running and removes it',
      () async {
        final process = await start(
          ['--worklog', '123'],
          holder('status'),
          env: {...environment(), 'HEAVY_LOCK_WORKLOG': '234'},
        );
        addTearDown(() => process.kill());
        var exited = false;
        unawaited(process.exitCode.then((_) => exited = true));
        final log = File('${temporary.path}${Platform.pathSeparator}log.txt');
        final startDeadline = DateTime.now().add(const Duration(seconds: 10));
        while (!exited &&
            DateTime.now().isBefore(startDeadline) &&
            (!(await log.exists()) ||
                !(await log.readAsString()).contains('status start'))) {
          await Future<void>.delayed(const Duration(milliseconds: 20));
        }
        expect(exited, isFalse, reason: 'runner rejected --worklog');
        final directory = Directory(
          '${temporary.path}${Platform.pathSeparator}heavy-lock.status',
        );
        final deadline = DateTime.now().add(const Duration(seconds: 5));
        List<FileSystemEntity> entries = [];
        while (DateTime.now().isBefore(deadline)) {
          if (await directory.exists()) {
            entries = await directory.list().toList();
            if (entries.whereType<File>().any(
              (f) => f.path.endsWith('.json'),
            )) {
              break;
            }
          }
          await Future<void>.delayed(const Duration(milliseconds: 20));
        }
        final records = entries.whereType<File>().where(
          (f) => f.path.endsWith('.json'),
        );
        expect(records, hasLength(1));
        final status = jsonDecode(
          await records.single.readAsString(),
        ) as Map<String, dynamic>;
        expect(status['worklog'], 123);
        expect(status['attribution'], 'declared');
        expect(status['state'], 'running');
        expect(status['capacity'], 2);
        expect(status['pid'], process.pid);
        expect(status['processStart'], isNotEmpty);
        if (Platform.isMacOS) {
          final permissions = await Process.run('/usr/bin/stat', [
            '-f',
            '%Lp',
            directory.path,
          ]);
          expect((permissions.stdout as String).trim(), '700');
        }
        release('status');
        expect((await finish(process)).$1, 0);
        expect(await records.single.exists(), isFalse);
      },
      skip: Platform.isWindows,
    );

    test('propagates the exit code and keeps the lock files', () async {
      final (code, _) = await finish(await start([], sleeper('a', 0, 3)));
      expect(code, 3);
      expect(File(gate).existsSync(), isTrue);
      expect(
        File(gate.replaceAll('.lock', '.slot-0.lock')).existsSync(),
        isTrue,
      );
    });

    test('rejects a missing command separator', () async {
      final (code, _) = await finish(await start(['--bogus'], ['x']));
      expect(code, usageExitCode);
    });

    test('serializes two heavy jobs and reports waiting', () async {
      final first = await start([], holder('first'));
      await waitForLog('first');
      final second = await start([], sleeper('second', 0));
      await waitForError(second, 'waiting for 2 of 2 slots');
      release('first');
      final (firstCode, _) = await finish(first);
      final (secondCode, secondErr) = await finish(second);
      expect([firstCode, secondCode], [0, 0]);
      expect(secondErr, contains('waiting for 2 of 2 slots'));
      final times = await intervals();
      expect(overlap(times['first']!, times['second']!), isFalse);
    });

    test('shares capacity between light jobs when resources allow', () async {
      final first = await start(['--weight', 'light'], holder('first'));
      await waitForLog('first');
      final second = await start(['--weight', 'light'], sleeper('second', 0));
      // The second job finishes while the first is still held.
      expect((await finish(second)).$1, 0);
      release('first');
      await finish(first);
      final times = await intervals();
      expect(overlap(times['first']!, times['second']!), isTrue);
    });

    test('light jobs run alone while resources are busy', () async {
      final busy = environment(maxCpu: '-1');
      final first = await start(
        ['--weight', 'light'],
        holder('first'),
        env: busy,
      );
      await waitForLog('first');
      final second = await start(
        ['--weight', 'light'],
        sleeper('second', 0),
        env: busy,
      );
      await waitForError(second, 'free resources');
      release('first');
      await finish(first);
      expect((await finish(second)).$1, 0);
      final times = await intervals();
      expect(overlap(times['first']!, times['second']!), isFalse);
    });

    test('a smaller capacity still sees jobs in higher slots', () async {
      final wide = environment(capacity: '4');
      final first = await start(
        ['--weight', 'heavy'],
        holder('first'),
        env: wide,
      );
      await waitForLog('first');
      final second = await start(
        ['--weight', 'light'],
        holder('second'),
        env: wide,
      );
      await waitForLog('second');
      release('first');
      await finish(first);
      // Slots 0 and 1 are free; slot 2 is held by the second job.
      final (code, err) = await finish(
        await start(
          ['--no-wait', '--weight', 'light'],
          sleeper('third', 0),
          env: environment(maxCpu: '-1'),
        ),
      );
      expect(code, busyExitCode, reason: err);
      release('second');
      await finish(second);
      expect((await intervals()).containsKey('third'), isFalse);
    });

    test('--no-wait exits busy while another job runs', () async {
      final first = await start([], holder('first'));
      await waitForLog('first');
      final (code, _) = await finish(
        await start(['--no-wait'], sleeper('second', 0)),
      );
      expect(code, busyExitCode);
      release('first');
      await finish(first);
      expect((await intervals()).containsKey('second'), isFalse);
    });

    test('nested runs inside an admitted command do not wait', () async {
      final nested = [dart, runner, '--', ...sleeper('inner', 0)];
      final (code, err) = await finish(await start([], nested));
      expect(code, 0);
      expect(err, contains('running directly'));
      expect((await intervals()).containsKey('inner'), isTrue);
    });

    test('excludes and is excluded by a legacy flock holder', () async {
      final legacy = await Process.start('python3', [
        '-c',
        'import fcntl,os,sys,time\n'
            'f=open(sys.argv[1],"a"); fcntl.flock(f, fcntl.LOCK_EX)\n'
            'print("held", flush=True)\n'
            'while not os.path.exists(sys.argv[2]): time.sleep(0.02)',
        gate,
        go('python'),
      ]);
      await legacy.stdout.transform(utf8.decoder).first;
      final waiting = await start([], sleeper('dart', 0));
      await waitForError(waiting, 'held exclusively');
      expect(
        await File('${temporary.path}${Platform.pathSeparator}log.txt')
            .exists(),
        isFalse,
      );
      release('python');
      final (code, _) = await finish(waiting);
      await legacy.exitCode;
      expect(code, 0);

      final running = await start([], holder('dart2'));
      await waitForLog('dart2');
      final probe = await Process.run('python3', [
        '-c',
        'import fcntl,sys\n'
            'f=open(sys.argv[1],"a")\n'
            'try:\n fcntl.flock(f, fcntl.LOCK_EX|fcntl.LOCK_NB); print("free")\n'
            'except BlockingIOError: print("busy")',
        gate,
      ]);
      expect((probe.stdout as String).trim(), 'busy');
      release('dart2');
      await finish(running);
    }, skip: Platform.isMacOS ? false : 'legacy flock interop is macOS-only');

    test('SIGTERM refreshes a snapshot taken before a child starts', () async {
      final bin = Directory('${temporary.path}/bin');
      await bin.create();
      final ps = File('${bin.path}/ps');
      await ps.writeAsString('''
#!/bin/sh
if [ "\$1" = "-A" ] && [ ! -e "\$RACE_DIR/used" ]; then
  touch "\$RACE_DIR/used"
  /bin/ps "\$@" > "\$RACE_DIR/snapshot"
  touch "\$RACE_DIR/captured"
  while [ ! -e "\$RACE_DIR/release" ]; do /bin/sleep 0.02; done
  cat "\$RACE_DIR/snapshot"
else
  exec /bin/ps "\$@"
fi
''');
      expect((await Process.run('/bin/chmod', ['755', ps.path])).exitCode, 0);
      final process = await start(
        [],
        [
          '/bin/sh',
          '-c',
          'while [ ! -e "\$RACE_DIR/go" ]; do /bin/sleep 0.02; done; '
              '/bin/sh -c \'i=0; while [ ! -e "\$RACE_DIR/stop" ] && [ "\$i" -lt 200 ]; do /bin/sleep 0.05; i=\$((i + 1)); done\' & '
              'echo \$! > "\$RACE_DIR/child"; wait',
        ],
        env: {
          ...environment(),
          'RACE_DIR': temporary.path,
          'PATH': '${bin.path}:${Platform.environment['PATH']}',
        },
      );
      addTearDown(() async {
        await File('${temporary.path}/release').writeAsString('');
        await File('${temporary.path}/stop').writeAsString('');
        if (await process.exitCode.timeout(
              const Duration(seconds: 5),
              onTimeout: () => -1,
            ) ==
            -1) {
          process.kill();
          await process.exitCode;
        }
        final child = File('${temporary.path}/child');
        if (await child.exists()) {
          final childPid = int.parse((await child.readAsString()).trim());
          final deadline = DateTime.now().add(const Duration(seconds: 5));
          while ((await Process.run('kill', ['-0', '$childPid'])).exitCode ==
                  0 &&
              DateTime.now().isBefore(deadline)) {
            await Future<void>.delayed(const Duration(milliseconds: 50));
          }
          expect(
            (await Process.run('kill', ['-0', '$childPid'])).exitCode,
            isNot(0),
            reason: 'test child did not exit after cleanup',
          );
        }
      });
      Future<void> waitForFile(String name) async {
        final file = File('${temporary.path}/$name');
        final deadline = DateTime.now().add(const Duration(seconds: 10));
        while (!await file.exists() && DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 20));
        }
        expect(await file.exists(), isTrue, reason: '$name was not created');
      }

      await waitForFile('captured');
      await File('${temporary.path}/go').writeAsString('');
      await waitForFile('child');
      final childPid = int.parse(
        (await File('${temporary.path}/child').readAsString()).trim(),
      );
      process.kill(ProcessSignal.sigterm);
      await Future<void>.delayed(const Duration(milliseconds: 200));
      await File('${temporary.path}/release').writeAsString('');
      expect((await finish(process)).$1, 143);
      final alive = await Process.run('kill', ['-0', '$childPid']);
      expect(alive.exitCode, isNot(0), reason: 'grandchild still running');
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test('SIGTERM stops the command tree before releasing the slots', () async {
      final grandchild = File(
        '${temporary.path}${Platform.pathSeparator}grandchild.pid',
      );
      final first = await start([], [
        'sh',
        '-c',
        'sleep 30 & echo \$! > ${grandchild.path}; wait',
      ]);
      while (!grandchild.existsSync() ||
          grandchild.readAsStringSync().trim().isEmpty) {
        await Future<void>.delayed(const Duration(milliseconds: 50));
      }
      final sleepPid = int.parse(grandchild.readAsStringSync().trim());
      // Let the runner see the whole tree before signalling (it polls `ps`
      // every second).
      await Future<void>.delayed(const Duration(milliseconds: 1500));
      first.kill(ProcessSignal.sigterm);
      final (code, _) = await finish(first);
      expect(code, 143);
      final alive = await Process.run('kill', ['-0', '$sleepPid']);
      expect(alive.exitCode, isNot(0), reason: 'grandchild still running');
      final (nextCode, _) = await finish(
        await start(['--no-wait'], sleeper('next', 0)),
      );
      expect(nextCode, 0);
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test('a second signal kills a survivor that ignored the first', () async {
      final survivor = File(
        '${temporary.path}${Platform.pathSeparator}survivor.pid',
      );
      // The survivor ignores SIGTERM, so the runner holds its slots for the
      // grace period unless another signal escalates.
      final first = await start([], [
        'sh',
        '-c',
        "sh -c 'trap \"\" TERM; echo \$\$ > ${survivor.path}; "
            "exec sleep 60' & sleep 60",
      ]);
      while (!survivor.existsSync() ||
          survivor.readAsStringSync().trim().isEmpty) {
        await Future<void>.delayed(const Duration(milliseconds: 50));
      }
      final survivorPid = int.parse(survivor.readAsStringSync().trim());
      // Let the runner see the whole tree before signalling.
      await Future<void>.delayed(const Duration(milliseconds: 1500));
      first.kill(ProcessSignal.sigterm);
      await Future<void>.delayed(const Duration(seconds: 2));
      expect(
        (await Process.run('kill', ['-0', '$survivorPid'])).exitCode,
        0,
        reason: 'the survivor ignores SIGTERM',
      );
      final (busyCode, _) = await finish(
        await start(['--no-wait'], sleeper('blocked', 0)),
      );
      expect(busyCode, busyExitCode, reason: 'slots held for the survivor');
      first.kill(ProcessSignal.sigterm);
      final (code, err) = await finish(first);
      expect(code, 143);
      expect(err, contains('stopping the command now'));
      expect(
        (await Process.run('kill', ['-0', '$survivorPid'])).exitCode,
        isNot(0),
        reason: 'survivor still running',
      );
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test('a child forked after the post-signal snapshot keeps the slots '
        'until it is stopped', () async {
      final bin = Directory('${temporary.path}/bin');
      await bin.create();
      final ps = File('${bin.path}/ps');
      // Holds the first two process-table snapshots until released, so the
      // test can fork a child after the runner's snapshot was taken.
      await ps.writeAsString('''
#!/bin/sh
if [ "\$1" = "-A" ]; then
  for n in 1 2; do
    if [ ! -e "\$RACE_DIR/used-\$n" ]; then
      touch "\$RACE_DIR/used-\$n"
      /bin/ps "\$@" > "\$RACE_DIR/snapshot-\$n"
      touch "\$RACE_DIR/captured-\$n"
      while [ ! -e "\$RACE_DIR/release-\$n" ]; do /bin/sleep 0.02; done
      cat "\$RACE_DIR/snapshot-\$n"
      exit 0
    fi
  done
fi
exec /bin/ps "\$@"
''');
      expect((await Process.run('/bin/chmod', ['755', ps.path])).exitCode, 0);
      final process = await start(
        [],
        [
          '/bin/sh',
          '-c',
          'while [ ! -e "\$RACE_DIR/go" ]; do /bin/sleep 0.02; done; '
              '/bin/sh -c \'trap "" TERM; echo \$\$ > "\$RACE_DIR/child"; i=0; while [ ! -e "\$RACE_DIR/stop" ] && [ "\$i" -lt 400 ]; do /bin/sleep 0.05; i=\$((i + 1)); done\' & '
              'wait',
        ],
        env: {
          ...environment(),
          'RACE_DIR': temporary.path,
          'PATH': '${bin.path}:${Platform.environment['PATH']}',
        },
      );
      Future<void> waitForFile(String name) async {
        final file = File('${temporary.path}/$name');
        final deadline = DateTime.now().add(const Duration(seconds: 10));
        while (!await file.exists() && DateTime.now().isBefore(deadline)) {
          await Future<void>.delayed(const Duration(milliseconds: 20));
        }
        expect(await file.exists(), isTrue, reason: '$name was not created');
      }

      Future<bool> alive(int pid) async =>
          (await Process.run('kill', ['-0', '$pid'])).exitCode == 0;
      addTearDown(() async {
        for (final name in ['release-1', 'release-2', 'go', 'stop']) {
          await File('${temporary.path}/$name').writeAsString('');
        }
        if (await process.exitCode.timeout(
              const Duration(seconds: 30),
              onTimeout: () => -1,
            ) ==
            -1) {
          process.kill(ProcessSignal.sigkill);
        }
      });

      // The runner's first snapshot is in progress when the signal arrives;
      // the second, taken after it, is the one the fork must not escape.
      await waitForFile('captured-1');
      process.kill(ProcessSignal.sigterm);
      await Future<void>.delayed(const Duration(milliseconds: 200));
      await File('${temporary.path}/release-1').writeAsString('');
      await waitForFile('captured-2');
      await File('${temporary.path}/go').writeAsString('');
      await waitForFile('child');
      final childPid = int.parse(
        (await File('${temporary.path}/child').readAsString()).trim(),
      );
      await File('${temporary.path}/release-2').writeAsString('');
      // The child ignores SIGTERM, so the runner must keep its slots.
      await Future<void>.delayed(const Duration(seconds: 2));
      expect(await alive(childPid), isTrue, reason: 'the child ignores TERM');
      final (busyCode, _) = await finish(
        await start(['--no-wait'], sleeper('blocked', 0)),
      );
      expect(busyCode, busyExitCode, reason: 'slots held for the child');
      process.kill(ProcessSignal.sigterm);
      expect((await finish(process)).$1, 143);
      expect(await alive(childPid), isFalse, reason: 'child still running');
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test('a descendant in a new session is still stopped', () async {
      final pidFile = File('${temporary.path}/escaped.pid');
      final first = await start([], [
        'sh',
        '-c',
        'python3 -c "import os, time; os.setsid(); '
            "open('${pidFile.path}', 'w').write(str(os.getpid())); "
            'time.sleep(30)" & wait',
      ]);
      while (!pidFile.existsSync() ||
          pidFile.readAsStringSync().trim().isEmpty) {
        await Future<void>.delayed(const Duration(milliseconds: 50));
      }
      final escaped = int.parse(pidFile.readAsStringSync().trim());
      await Future<void>.delayed(const Duration(milliseconds: 1500));
      first.kill(ProcessSignal.sigterm);
      expect((await finish(first)).$1, 143);
      expect(
        (await Process.run('kill', ['-0', '$escaped'])).exitCode,
        isNot(0),
        reason: 'setsid descendant still running',
      );
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test('cancelling an outer run stops a nested run and its tree', () async {
      final pidFile = File('${temporary.path}/nested.pid');
      final outer = await start([], [
        dart,
        runner,
        '--',
        'sh',
        '-c',
        'sleep 30 & echo \$! > ${pidFile.path}; wait',
      ]);
      while (!pidFile.existsSync() ||
          pidFile.readAsStringSync().trim().isEmpty) {
        await Future<void>.delayed(const Duration(milliseconds: 50));
      }
      final sleepPid = int.parse(pidFile.readAsStringSync().trim());
      await Future<void>.delayed(const Duration(milliseconds: 1500));
      outer.kill(ProcessSignal.sigterm);
      final (code, err) = await finish(outer);
      expect(code, 143);
      expect(err, contains('already admitted by a parent run'));
      expect(
        (await Process.run('kill', ['-0', '$sleepPid'])).exitCode,
        isNot(0),
        reason: 'nested grandchild still running',
      );
      final (nextCode, _) = await finish(
        await start(['--no-wait'], sleeper('next', 0)),
      );
      expect(nextCode, 0);
    }, skip: Platform.isWindows ? 'POSIX signals only' : false);

    test(
      'Ctrl+C in a terminal reaches the command, which restores the '
      'terminal',
      () async {
        // Runs the runner with a pseudo-terminal as its controlling terminal,
        // types a key (as for hot reload), then Ctrl+C.
        final script = File('${temporary.path}/terminal_check.py');
        await script.writeAsString(r'''
import fcntl, os, pty, select, subprocess, sys, termios, time
master, slave = pty.openpty()
def controlling():
    os.setsid()
    fcntl.ioctl(0, termios.TIOCSCTTY, 0)
command = ["sh", "-c",
    "stty -icanon -echo; trap 'stty icanon echo; "
    "echo restored:$(stty -a | tr \" \" \"\\n\" | grep -cxE \"icanon|echo\"); "
    "exit 130' INT; "
    "echo ready; key=$(dd bs=1 count=1 2>/dev/null); echo \"key:$key\"; "
    "while :; do sleep 0.05; done"]
child = subprocess.Popen(sys.argv[1:] + ["--"] + command, stdin=slave,
    stdout=slave, stderr=slave, preexec_fn=controlling)
output = b""
def expect(text, start=0):
    global output
    deadline = time.time() + 20
    while text not in output[start:]:
        if time.time() > deadline:
            sys.exit("timed out waiting for %r in %r" % (text, output))
        if select.select([master], [], [], 0.1)[0]:
            output += os.read(master, 1024)
expect(b"ready")
os.write(master, b"r")
expect(b"key:r")
time.sleep(1.5)
os.write(master, b"\x03")
expect(b"restored:")
code = child.wait(timeout=30)
expect(b"\n", start=output.index(b"restored:"))
print(code, output[output.index(b"restored:"):].split()[0].decode())
''');
        final result = await Process.run(
          'python3',
          [script.path, dart, runner],
          environment: environment(),
          includeParentEnvironment: false,
        );
        expect(result.stderr, isEmpty);
        expect((result.stdout as String).trim(), '130 restored:2');
      },
      timeout: const Timeout(Duration(minutes: 2)),
      skip: Platform.isWindows ? 'POSIX terminals only' : false,
    );
  });
}

/// A deterministic model of the POSIX process behavior [ProcessTree] relies
/// on: stops take effect at the next scheduling point (here, during the next
/// snapshot), stopped processes neither fork nor exit, and the orphans of an
/// exiting process are reparented to pid 1.
final class _FakeKernel {
  final _processes = <int, _FakeProcess>{1: _FakeProcess(0)};
  final log = <String>[];
  var _next = 100;

  /// Runs after a snapshot is taken, before [ProcessTree] acts on it.
  void Function()? afterSnapshot;

  /// Runs as a pending stop takes effect, after `ps` listed the processes.
  void Function(int pid)? beforeStop;

  Set<int> get alive => this._processes.keys.toSet();

  bool isStopped(int pid) => this._processes[pid]?.stopped ?? false;

  int spawn(int parent, {bool ignoresTerm = false, bool stoppable = true}) {
    final pid = this._next++;
    this._processes[pid] = _FakeProcess(parent)
      ..ignoresTerm = ignoresTerm
      ..stoppable = stoppable;
    return pid;
  }

  ProcessTree tree(int root, {Duration? freezeLimit}) => ProcessTree(
    root,
    snapshot: this.snapshot,
    kill: this.kill,
    freezeLimit: freezeLimit ?? const Duration(seconds: 2),
  );

  Future<Map<int, ProcessEntry>?> snapshot() async {
    final listed = this._processes.keys.toList();
    for (final MapEntry(:key, :value) in this._processes.entries.toList()) {
      if (value.stopRequested && !value.stopped) {
        this.beforeStop?.call(key);
        value.stopped = true;
      }
    }
    final table = {
      for (final pid in listed)
        if (this._processes[pid] case final process?)
          pid: (
            parent: process.parent,
            state: process.stopped ? 'T' : 'S',
            started: 'start-$pid',
          ),
    };
    this.afterSnapshot?.call();
    await Future<void>.delayed(Duration.zero);
    return table;
  }

  bool kill(int pid, ProcessSignal signal) {
    final process = this._processes[pid];
    if (process == null) {
      return false;
    }
    this.log.add('$pid $signal');
    switch (signal) {
      case ProcessSignal.sigstop:
        process.stopRequested = process.stoppable;
      case ProcessSignal.sigcont:
        process
          ..stopRequested = false
          ..stopped = false;
        if (process.terminating) {
          this._exit(pid);
        }
      case ProcessSignal.sigkill:
        this._exit(pid);
      default:
        if (!process.ignoresTerm) {
          if (process.stopRequested || process.stopped) {
            process.terminating = true;
          } else {
            this._exit(pid);
          }
        }
    }
    return true;
  }

  void _exit(int pid) {
    this._processes.remove(pid);
    for (final MapEntry(:key, :value) in this._processes.entries.toList()) {
      if (value.parent == pid) {
        value.parent = 1;
        // Models a stopped process group orphaned by its parent's exit,
        // which the kernel hangs up.
        if (value.stopped || value.stopRequested) {
          this.log.add('$key SIGHUP');
          this._exit(key);
        }
      }
    }
  }
}

final class _FakeProcess {
  _FakeProcess(this.parent);

  int parent;
  var ignoresTerm = false;
  var stoppable = true;
  var stopRequested = false;
  var stopped = false;
  var terminating = false;
}
