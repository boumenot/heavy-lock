import 'dart:io';

import 'package:heavy_lock_tool/dev.dart';
import 'package:test/test.dart';

/// Records commands; `dart compile` and `ditto` create their outputs so the
/// orchestrator's staging and replacement can be observed. Git metadata is
/// read from real temporary repositories.
class FakeShell implements Shell {
  final calls = <List<String>>[];
  final environments = <Map<String, String>?>[];
  final failing = <String>{};
  final failingArguments = <String>{};

  @override
  Future<int> run(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
    Map<String, String>? environment,
  }) async {
    final call = [executable.split('/').last, ...arguments];
    this.calls.add(call);
    this.environments.add(environment);
    final key = call.take(2).join(' ');
    if (this.failing.contains(key) ||
        call.any(this.failingArguments.contains)) {
      return 3;
    }
    if (key == 'dart compile') {
      await File(arguments.last).writeAsString('binary ${arguments[4]}');
    } else if (key == 'go build') {
      await File(arguments[3]).writeAsString('tuna');
    } else if (key.startsWith('ditto')) {
      await _copyDirectory(Directory(arguments[0]), arguments[1]);
    }
    return 0;
  }

  @override
  Future<(int, String)> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  }) => const ProcessShell().capture(
    executable,
    arguments,
    workingDirectory: workingDirectory,
  );

  List<String> get steps => [
    for (final call in this.calls) call.take(2).join(' '),
  ];
}

Future<void> _copyDirectory(Directory source, String target) async {
  await for (final entity in source.list(recursive: true)) {
    final path = '$target/${entity.path.substring(source.path.length + 1)}';
    if (entity is File) {
      await File(path).create(recursive: true);
      await entity.copy(path);
    }
  }
}

void main() {
  late Directory temporary;
  late String root;
  late FakeShell shell;
  late StringBuffer out;
  late StringBuffer err;

  Future<void> git(List<String> arguments) async {
    final result = await Process.run('git', [
      '-c',
      'user.name=Test',
      '-c',
      'user.email=test@example.com',
      '-c',
      'commit.gpgsign=false',
      ...arguments,
    ], workingDirectory: root);
    expect(result.exitCode, 0, reason: '${result.stderr}');
  }

  Future<String> head() async =>
      ((await Process.run('git', [
                'rev-parse',
                '--short=12',
                'HEAD',
              ], workingDirectory: root)).stdout
              as String)
          .trim();

  DevContext context(String os) => DevContext(
    root: root,
    os: os,
    dart: '/sdk/bin/dart',
    home: '${temporary.path}/home',
    shell: shell,
    out: out,
    err: err,
  );

  Future<int> dev(List<String> arguments, {String os = 'macos'}) =>
      runDev(arguments, context(os));

  setUp(() async {
    temporary = await Directory.systemTemp.createTemp('heavy_lock_dev_');
    root = '${temporary.path}/checkout';
    await Directory('$root/apps/heavy-lock').create(recursive: true);
    await File('$root/apps/heavy-lock/pubspec.yaml')
        .writeAsString('name: heavy_lock\nversion: 2.3.0\n');
    await File('$root/.gitignore').writeAsString('build/\n');
    await git(['init', '-q', '-b', 'master']);
    await git(['add', '-A']);
    await git(['commit', '-q', '-m', 'first']);
    shell = FakeShell();
    out = StringBuffer();
    err = StringBuffer();
  });

  tearDown(() => temporary.delete(recursive: true));

  group('targets', () {
    test('build compiles with the version stamp and runs no tests', () async {
      expect(await dev(['build', 'heavy-lock']), 0, reason: '$err');
      final hash = await head();
      expect(shell.steps, ['dart compile', 'dart compile']);
      expect(shell.calls.first.sublist(3, 6), [
        '-Dheavy_lock.version=2.3.1+$hash',
        '-Dheavy_lock.build=master build',
        'bin/heavy_lock.dart',
      ]);
      expect(
        await File('$root/build/heavy-lock/heavy-lock').readAsString(),
        'binary bin/heavy_lock.dart',
      );
      expect(
        await File('$root/build/heavy-lock/heavy-lock-status').readAsString(),
        'binary bin/heavy_lock_status.dart',
      );
      expect(
        await Directory('$root/build/heavy-lock').list().length,
        2,
        reason: 'no staged files remain',
      );
    });

    test('a failed compile keeps the previous executables', () async {
      await Directory('$root/build/heavy-lock').create(recursive: true);
      final previous = File('$root/build/heavy-lock/heavy-lock');
      await previous.writeAsString('previous');
      shell.failing.add('dart compile');

      expect(await dev(['build', 'heavy-lock']), isNot(0));
      expect(await previous.readAsString(), 'previous');
      expect(await Directory('$root/build/heavy-lock').list().length, 1);
      expect('$err', contains('existing executables were not replaced'));
    });

    test('test runs only the selected tests', () async {
      expect(await dev(['test', 'heavy-lock']), 0);
      expect(shell.steps, ['dart test']);

      shell.calls.clear();
      shell.environments.clear();
      expect(await dev(['test', 'monitor-macos']), 0);
      expect(shell.steps, ['swift test']);
      expect(shell.environments.single, {
        'HEAVY_LOCK_MONITOR_DART': '/sdk/bin/dart',
      });
    });

    test('check runs build and test and reports each failure', () async {
      shell.failing.add('dart compile');
      expect(await dev(['check', 'heavy-lock']), 1);
      expect(shell.steps, ['dart compile', 'dart test']);
      expect('$err', contains('Failed: build heavy-lock (exit 3)'));
      expect('$err', isNot(contains('test heavy-lock')));
    });

    test('all selects implemented applications for the host', () async {
      expect(await dev(['test', 'all'], os: 'linux'), 0);
      expect(shell.steps, ['dart test']);

      shell.calls.clear();
      expect(await dev(['test', 'all']), 0);
      expect(shell.steps, [
        'dart test',
        'swift test',
        'go test',
        'python3 $root/docs/v2/model/check.py',
        'swift test',
        'swift test',
      ]);
      expect(shell.calls[4].last, '$root/packages/heavylock-v2-swift');
      expect(shell.calls[4], contains(SwiftContractApp.resolvedOnly));
      expect(shell.calls.last.last, '$root/apps/tuna-monitor-macos');
    });

    test(
      'tuna test runs the bounded lifecycle models and surfaces failure',
      () async {
        expect(await dev(['test', 'tuna']), 0);
        expect(shell.steps, [
          'go test',
          'python3 $root/docs/v2/model/check.py',
        ]);
        expect(shell.calls.last, contains('DescendantScanSafety'));
        expect(shell.calls.last, contains('DescendantScanNoBarrier'));
        expect(shell.calls.last, contains('DescendantScanUnstable'));
        expect(shell.calls.last, contains('RunnerFinishSafety'));
        expect(shell.calls.last, contains('RunnerFinishBlocking'));
        expect(shell.calls.last, contains('RunnerFinishDropped'));
        expect(
          shell.calls.last,
          containsAll([
            'MonitorDelivery',
            'MonitorLoss',
            'MonitorRestart',
            'MonitorStorage',
            'MonitorGap',
            'MonitorUnknownSafe',
            'MonitorEmptySafe',
            'MonitorStaleLive',
            'MonitorStaleMode',
            'MonitorUnknownMode',
            'MonitorBlindReplay',
            'MonitorUnknownEvent',
            'MonitorEmptyFirst',
            'MonitorReplayReached',
            'MonitorGapReached',
            '--timeout',
          ]),
        );

        shell.calls.clear();
        shell.failing.add('python3 $root/docs/v2/model/check.py');
        expect(await dev(['test', 'tuna']), 1);
        expect('$err', contains('Failed: test tuna (exit 3)'));

        shell.calls.clear();
        shell.failing.add('go test');
        expect(await dev(['test', 'tuna']), 1);
        expect(shell.steps, ['go test']);
      },
    );

    test('tuna builds atomically and is never installed', () async {
      expect(await dev(['build', 'tuna']), 0, reason: '$err');
      expect(shell.steps, ['go build', 'go build']);
      expect(shell.calls.first, isNot(contains('-tags')));
      expect(
        shell.calls.last,
        containsAllInOrder(['-tags', TunaApp.testBrokerTag]),
      );
      expect(await File('$root/build/tuna/tuna').readAsString(), 'tuna');
      expect(
        await File('$root/build/tuna/${TunaApp.testBroker}').exists(),
        isTrue,
      );
      expect(await Directory('$root/build/tuna').list().length, 2);

      shell.failing.add('go build');
      await File('$root/build/tuna/tuna').writeAsString('previous');
      expect(await dev(['build', 'tuna']), isNot(0));
      expect(await File('$root/build/tuna/tuna').readAsString(), 'previous');
      expect(await Directory('$root/build/tuna').list().length, 2);

      shell.calls.clear();
      expect(await dev(['install', 'tuna']), unavailableExitCode);
      expect('$err', contains('tuna is not installable'));
      expect(await dev(['install', 'heavylock-v2-swift']), unavailableExitCode);
      expect(await dev(['install', 'tuna-monitor-macos']), unavailableExitCode);
      expect(shell.calls, isEmpty);
    });

    test('a failed test broker build keeps both previous binaries', () async {
      final output = Directory('$root/build/tuna');
      await output.create(recursive: true);
      final production = File('${output.path}/tuna');
      final broker = File('${output.path}/${TunaApp.testBroker}');
      await production.writeAsString('previous tuna');
      await broker.writeAsString('previous test broker');
      shell.failingArguments.add(TunaApp.testBrokerTag);

      expect(await dev(['build', 'tuna']), isNot(0));
      expect(await production.readAsString(), 'previous tuna');
      expect(await broker.readAsString(), 'previous test broker');
      expect(await output.list().length, 2);
      expect('$err', contains('existing executables were not replaced'));
    });

    test(
      'the v2 monitor builds a signed bundle from pinned versions',
      () async {
        final package = '$root/apps/tuna-monitor-macos';
        await File('$package/.build/release/TunaMonitor')
            .create(recursive: true);
        await File('$package/Info.plist').writeAsString('plist');
        expect(await dev(['build', 'tuna-monitor-macos']), 0, reason: '$err');
        expect(shell.steps, ['swift build', 'chmod 755', 'codesign --force']);
        expect(shell.calls.first, contains(SwiftContractApp.resolvedOnly));
        final bundle = '$root/build/tuna-monitor-macos/Tuna Monitor.app';
        expect(
          await File('$bundle/Contents/Info.plist').readAsString(),
          'plist',
        );
        expect(
          await File('$bundle/Contents/MacOS/TunaMonitor').exists(),
          isTrue,
        );
        expect(
          await Directory('$root/build/tuna-monitor-macos').list().length,
          1,
          reason: 'no staging directories remain',
        );
      },
    );

    test('the v2 monitor tests a built tuna when available', () async {
      expect(await dev(['test', 'tuna-monitor-macos']), 0);
      expect(shell.environments.single, isNull);
      expect('$out', contains('Skipping the broker integration test'));

      // A production broker would take the account's real singleton lock.
      await File('$root/build/tuna/tuna').create(recursive: true);
      shell.environments.clear();
      expect(await dev(['test', 'tuna-monitor-macos']), 0);
      expect(shell.environments.single, isNull);

      final broker = '$root/build/tuna/${TunaApp.testBroker}';
      await File(broker).create(recursive: true);
      shell.environments.clear();
      expect(await dev(['test', 'tuna-monitor-macos']), 0);
      expect(shell.steps.last, 'swift test');
      expect(shell.calls.last, contains(SwiftContractApp.resolvedOnly));
      expect(shell.environments.single, {'TUNA_MONITOR_TEST_TUNA': broker});
    });

    test('install all skips applications that are not installable', () async {
      await File(
        '$root/build/monitor-macos/Heavy Lock Status.app/'
        'Contents/MacOS/HeavyLockMonitor',
      ).create(recursive: true);
      expect(await dev(['install', 'all']), 0, reason: '$err');
      expect('$out', isNot(contains('tuna')));
      expect('$out', isNot(contains('heavylock-v2-swift')));
      expect(shell.steps.first, 'bash $root/tool/install.sh');
    });

    test('unknown, future and unsupported targets fail clearly', () async {
      expect(await dev(['build', 'nope']), usageExitCode);
      expect('$err', contains('Unknown application: nope'));
      expect(await dev(['build', 'monitor-windows']), unavailableExitCode);
      expect('$err', contains('monitor-windows is not implemented yet'));
      expect(
        await dev(['install', 'monitor-macos'], os: 'linux'),
        unavailableExitCode,
      );
      expect('$err', contains('not supported on linux'));
      expect(await dev(['install', 'all', '/tmp/x']), usageExitCode);
      expect(await dev(['build']), usageExitCode);
      expect(shell.calls, isEmpty);
    });

    test('list distinguishes implemented, unsupported and future', () async {
      expect(await dev(['list'], os: 'linux'), 0);
      expect('$out', contains(RegExp(r'heavy-lock +implemented')));
      expect('$out', contains('implemented, unsupported on linux'));
      expect('$out', contains(RegExp(r'monitor-linux +future')));
    });
  });

  group('monitor install', () {
    late String built;
    late String destination;

    setUp(() async {
      built = '$root/build/monitor-macos/Heavy Lock Status.app';
      await File('$built/Contents/MacOS/HeavyLockMonitor')
          .create(recursive: true);
      await File('$built/Contents/MacOS/HeavyLockMonitor').writeAsString('new');
      destination = '${temporary.path}/Applications';
    });

    test('upgrades an existing bundle in place', () async {
      final installed = File(
        '$destination/Heavy Lock Status.app/Contents/MacOS/HeavyLockMonitor',
      );
      await installed.create(recursive: true);
      await installed.writeAsString('old');
      await File('$destination/Heavy Lock Status.app/stale')
          .writeAsString('stale');

      expect(await dev(['install', 'monitor-macos', destination]), 0);
      expect(await installed.readAsString(), 'new');
      expect(
        await File('$destination/Heavy Lock Status.app/stale').exists(),
        isFalse,
      );
      expect(await Directory(destination).list().map((e) => e.path).toList(), [
        '$destination/Heavy Lock Status.app',
      ]);
    });

    test('a failed copy keeps the existing bundle', () async {
      final installed = File(
        '$destination/Heavy Lock Status.app/Contents/MacOS/HeavyLockMonitor',
      );
      await installed.create(recursive: true);
      await installed.writeAsString('old');
      shell.failing.add('ditto $built');

      expect(await dev(['install', 'monitor-macos', destination]), isNot(0));
      expect(await installed.readAsString(), 'old');
      expect(await Directory(destination).list().map((e) => e.path).toList(), [
        '$destination/Heavy Lock Status.app',
      ]);
    });

    test('defaults to ~/Applications and requires a build', () async {
      expect(await dev(['install', 'monitor-macos']), 0, reason: '$err');
      expect(
        await File(
          '${temporary.path}/home/Applications/Heavy Lock Status.app/'
          'Contents/MacOS/HeavyLockMonitor',
        ).readAsString(),
        'new',
      );

      await Directory(built).delete(recursive: true);
      expect(await dev(['install', 'monitor-macos']), 1);
      expect('$err', contains('build monitor-macos'));
    });
  });

  group('version', () {
    Future<List<String>> version() async {
      out.clear();
      expect(await dev(['version']), 0, reason: '$err');
      return '$out'.trimRight().split('\n');
    }

    test('a clean master commit is a master build', () async {
      expect(await version(), ['2.3.1+${await head()}', 'master build']);
    });

    test('every commit bumps the patch number', () async {
      await File('$root/change').writeAsString('change');
      await git(['add', '-A']);
      await git(['commit', '-q', '-m', 'second']);
      expect(await version(), ['2.3.2+${await head()}', 'master build']);
    });

    test('a commit not on master is dirty and names the branch', () async {
      await git(['checkout', '-q', '-b', 'feature']);
      await File('$root/change').writeAsString('change');
      await git(['add', '-A']);
      await git(['commit', '-q', '-m', 'feature']);
      expect(await version(), [
        '2.3.2-dirty+${await head()}',
        'non-master build: commit not on master (branch feature)',
      ]);
    });

    test('uncommitted changes make a master commit dirty', () async {
      await File('$root/untracked').writeAsString('change');
      expect(await version(), [
        '2.3.1-dirty+${await head()}',
        'non-master build: uncommitted changes',
      ]);
    });

    test('a directory outside Git is reported as such', () async {
      await Directory('$root/.git').delete(recursive: true);
      expect(await version(), [
        '2.3.0-dirty+unknown',
        'non-master build: not a git checkout',
      ]);
    });
  });
}
