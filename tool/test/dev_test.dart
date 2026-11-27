import 'dart:io';

import 'package:tuna_tool/dev.dart';
import 'package:test/test.dart';

/// Records commands; `go build` creates its output so the orchestrator's
/// staging and replacement can be observed.
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
    if (key == 'go build') {
      await File(arguments[3]).writeAsString('tuna');
    }
    return 0;
  }

  List<String> get steps => [
    for (final call in this.calls) call.take(2).join(' '),
  ];
}

void main() {
  late Directory temporary;
  late String root;
  late FakeShell shell;
  late StringBuffer out;
  late StringBuffer err;

  DevContext context(String os) => DevContext(
    root: root,
    os: os,
    dart: '/sdk/bin/dart',
    shell: shell,
    out: out,
    err: err,
  );

  Future<int> dev(List<String> arguments, {String os = 'macos'}) =>
      runDev(arguments, context(os));

  setUp(() async {
    temporary = await Directory.systemTemp.createTemp('tuna_dev_');
    root = '${temporary.path}/checkout';
    await Directory(root).create(recursive: true);
    shell = FakeShell();
    out = StringBuffer();
    err = StringBuffer();
  });

  tearDown(() => temporary.delete(recursive: true));

  group('targets', () {
    test('check runs build and test and reports each failure', () async {
      shell.failing.add('go build');
      expect(await dev(['check', 'tuna']), 1);
      expect(shell.steps, [
        'go build',
        'go test',
        'python3 $root/docs/model/check.py',
      ]);
      expect('$err', contains('Failed: build tuna (exit 3)'));
      expect('$err', isNot(contains('test tuna')));
    });

    test('all selects implemented applications for the host', () async {
      expect(await dev(['test', 'all'], os: 'linux'), 0);
      expect(shell.steps, isEmpty);

      expect(await dev(['test', 'all']), 0);
      expect(shell.steps, [
        'go test',
        'python3 $root/docs/model/check.py',
        'swift test',
        'swift test',
      ]);
      expect(shell.calls[2].last, '$root/packages/tuna-contract-swift');
      expect(shell.calls[2], contains(SwiftContractApp.resolvedOnly));
      expect(shell.calls.last.last, '$root/apps/tuna-monitor-macos');
    });

    test(
      'tuna test runs the bounded lifecycle models and surfaces failure',
      () async {
        expect(await dev(['test', 'tuna']), 0);
        expect(shell.steps, ['go test', 'python3 $root/docs/model/check.py']);
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
        shell.failing.add('python3 $root/docs/model/check.py');
        expect(await dev(['test', 'tuna']), 1);
        expect('$err', contains('Failed: test tuna (exit 3)'));

        shell.calls.clear();
        shell.failing.add('go test');
        expect(await dev(['test', 'tuna']), 1);
        expect(shell.steps, ['go test']);
      },
    );

    test('tuna builds atomically', () async {
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

    test('the monitor builds a signed bundle from pinned versions', () async {
      final package = '$root/apps/tuna-monitor-macos';
      await File('$package/.build/release/TunaMonitor').create(recursive: true);
      await File('$package/Info.plist').writeAsString('plist');
      expect(await dev(['build', 'tuna-monitor-macos']), 0, reason: '$err');
      expect(shell.steps, ['swift build', 'chmod 755', 'codesign --force']);
      expect(shell.calls.first, contains(SwiftContractApp.resolvedOnly));
      final bundle = '$root/build/tuna-monitor-macos/Tuna Monitor.app';
      expect(await File('$bundle/Contents/Info.plist').readAsString(), 'plist');
      expect(await File('$bundle/Contents/MacOS/TunaMonitor').exists(), isTrue);
      expect(
        await Directory('$root/build/tuna-monitor-macos').list().length,
        1,
        reason: 'no staging directories remain',
      );
    });

    test('the monitor tests a built tuna when available', () async {
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

    test('unknown, future and unsupported targets fail clearly', () async {
      expect(await dev(['build', 'nope']), usageExitCode);
      expect('$err', contains('Unknown application: nope'));
      expect(await dev(['build', 'monitor-windows']), unavailableExitCode);
      expect('$err', contains('monitor-windows is not implemented yet'));
      expect(await dev(['build', 'tuna'], os: 'linux'), unavailableExitCode);
      expect('$err', contains('not supported on linux'));
      expect(await dev(['install', 'tuna']), usageExitCode);
      expect(await dev(['version']), usageExitCode);
      expect(await dev(['build']), usageExitCode);
      expect(shell.calls, isEmpty);
    });

    test('list distinguishes implemented, unsupported and future', () async {
      expect(await dev(['list'], os: 'linux'), 0);
      expect(
        '$out',
        contains(RegExp(r'tuna +implemented, unsupported on linux')),
      );
      expect('$out', contains(RegExp(r'monitor-linux +future')));
    });
  });
}
