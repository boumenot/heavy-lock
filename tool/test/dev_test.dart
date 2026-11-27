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
  final running = <String>{};
  final captured = <List<String>>[];
  void Function(List<String>)? onCapture;
  bool invalidNative = false;
  int? probeExitCode;

  @override
  Future<ProcessResult> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  }) async {
    final call = [executable.split('/').last, ...arguments];
    this.captured.add(call);
    this.onCapture?.call(call);
    if (call.first == 'pgrep') {
      return ProcessResult(
        0,
        this.probeExitCode ?? (this.running.contains(arguments.last) ? 0 : 1),
        '',
        '',
      );
    }
    if (call.first == 'file') {
      return ProcessResult(
        0,
        0,
        this.invalidNative ? 'ASCII text' : 'Mach-O 64-bit executable arm64',
        '',
      );
    }
    return const ProcessShell().capture(
      executable,
      arguments,
      workingDirectory: workingDirectory,
    );
  }

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
      await const ProcessShell().run('chmod', ['755', arguments[3]]);
    }
    if (call.first == 'chmod')
      return const ProcessShell().run(executable, arguments);
    return 0;
  }

  List<String> get steps => [
    for (final call in this.calls) call.take(2).join(' '),
  ];
}

class ExtraMacApp extends App {
  const ExtraMacApp();

  @override
  String get name => 'extra-mac-app';
  @override
  String get description => 'Another installable macOS application';
  @override
  Set<String> get hosts => {'macos'};
  @override
  Future<int> build(DevContext context) async => 0;
  @override
  Future<int> test(DevContext context) async => 0;
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
    home: '${temporary.path}/home',
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
      expect(await dev(['test', 'all'], os: 'linux'), unavailableExitCode);
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
            'RunnerExitSafety',
            'RunnerExitFailureSafety',
            'RunnerExitOldRule',
            'RunnerExitNoBarrier',
            'RunnerExitCleanRelease',
            'RunnerExitDeathTakeover',
          ]),
        );
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
      expect(await dev(['install', 'tuna']), unavailableExitCode);
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

  group('installation', () {
    Future<void> checkedApps() async {
      final package = '$root/apps/tuna-monitor-macos';
      await File('$package/.build/release/TunaMonitor').create(recursive: true);
      await File('$package/Info.plist').writeAsString('plist');
      expect(await dev(['check', 'all']), 0, reason: '$err');
      shell.calls.clear();
      shell.captured.clear();
    }

    String installedTuna() => '${temporary.path}/home/.local/bin/tuna';
    String installedMonitor() =>
        '${temporary.path}/home/Applications/Tuna Monitor.app/Contents/MacOS/TunaMonitor';

    test(
      'all requires a successful aggregate check before installation',
      () async {
        expect(await dev(['install', 'all']), isNot(0));
        expect('$err', contains('check all'));
        expect(shell.calls, isEmpty);
      },
    );

    test(
      'aggregate check includes every host target and integration broker',
      () async {
        await checkedApps();
        expect('$out', contains('==> build tuna'));
        expect('$out', contains('==> test tuna'));
        expect('$out', contains('==> build tuna-contract-swift'));
        expect('$out', contains('==> test tuna-contract-swift'));
        expect('$out', contains('==> build tuna-monitor-macos'));
        expect('$out', contains('==> test tuna-monitor-macos'));
        expect(shell.environments.whereType<Map<String, String>>().last, {
          'TUNA_MONITOR_TEST_TUNA': '$root/build/tuna/${TunaApp.testBroker}',
        });
        expect('$out', contains('Skipping monitor-windows: not implemented'));
        expect('$out', contains('Skipping monitor-linux: not implemented'));
      },
    );

    test(
      'first installation deploys both checked apps but not the test broker',
      () async {
        await checkedApps();
        expect(await dev(['install', 'all']), 0, reason: '$err');
        expect(await File(installedTuna()).readAsString(), 'tuna');
        expect(await File(installedMonitor()).exists(), isTrue);
        expect((await File(installedTuna()).stat()).mode & 0x49, 0x49);
        expect(
          await File('${temporary.path}/home/.local/bin/tuna-test-broker')
              .exists(),
          isFalse,
        );
        expect(shell.steps, [
          'plutil -lint',
          'codesign --verify',
          'plutil -lint',
          'codesign --verify',
        ]);
        expect(
          '$out',
          contains('No services, processes, data or login settings changed'),
        );
      },
    );

    test('replacement upgrades both existing apps', () async {
      await checkedApps();
      expect(await dev(['install', 'all']), 0);
      await File(installedTuna()).writeAsString('old runner');
      await File(installedMonitor()).writeAsString('old monitor');
      expect(await dev(['install', 'all']), 0, reason: '$err');
      expect(await File(installedTuna()).readAsString(), 'tuna');
      expect(await File(installedMonitor()).readAsString(), '');
    });

    test(
      'missing or modified artifacts preserve both installed apps',
      () async {
        await checkedApps();
        expect(await dev(['install', 'all']), 0);
        await File(installedTuna()).writeAsString('old runner');
        await File(installedMonitor()).writeAsString('old monitor');
        final tuna = File('$root/build/tuna/tuna');
        await tuna.writeAsString('modified');
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Artifacts changed'));
        await tuna.delete();
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Missing regular executable'));
        expect(await File(installedTuna()).readAsString(), 'old runner');
        expect(await File(installedMonitor()).readAsString(), 'old monitor');
      },
    );

    test('a failed recheck revokes previous installation validation', () async {
      await checkedApps();
      shell.failing.add('go test');
      expect(await dev(['check', 'all']), 1);
      expect(await dev(['install', 'all']), unavailableExitCode);
      expect(await File(installedTuna()).exists(), isFalse);
    });

    test(
      'running apps and legacy symlinks are refused without changing them',
      () async {
        await checkedApps();
        shell.running.add('TunaMonitor');
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('TunaMonitor is running'));
        expect(await File(installedTuna()).exists(), isFalse);
        shell.running.clear();
        await File('${temporary.path}/legacy-tuna').writeAsString('legacy');
        await Directory(File(installedTuna()).parent.path)
            .create(recursive: true);
        await Link(installedTuna()).create('${temporary.path}/legacy-tuna');
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Legacy runner symlinks'));
        expect(await File(installedTuna()).readAsString(), 'legacy');
        expect(await File(installedMonitor()).exists(), isFalse);
      },
    );

    test(
      'signature failure and stopped-state recheck preserve previous apps',
      () async {
        await checkedApps();
        expect(await dev(['install', 'all']), 0);
        await File(installedTuna()).writeAsString('old runner');
        await File(installedMonitor()).writeAsString('old monitor');
        shell.failing.add('codesign --verify');
        expect(await dev(['install', 'all']), 1);
        shell.failing.clear();
        var checks = 0;
        shell.onCapture = (call) {
          if (call.first == 'pgrep' && ++checks == 3) shell.running.add('tuna');
        };
        expect(await dev(['install', 'all']), 1);
        expect(await File(installedTuna()).readAsString(), 'old runner');
        expect(await File(installedMonitor()).readAsString(), 'old monitor');
      },
    );

    test('a second swap failure rolls back both apps', () async {
      await checkedApps();
      expect(await dev(['install', 'all']), 0);
      await File(installedTuna()).writeAsString('old runner');
      await File(installedMonitor()).writeAsString('old monitor');
      var checks = 0;
      shell.onCapture = (call) {
        if (call.first == 'pgrep' && ++checks == 4) {
          final parent = Directory('${temporary.path}/home/Applications');
          final staging = parent.listSync().whereType<Directory>().singleWhere(
            (entry) => entry.path.split('/').last.startsWith('.tuna-install-'),
          );
          Directory('${staging.path}/Tuna Monitor.app')
              .deleteSync(recursive: true);
        }
      };
      expect(await dev(['install', 'all']), 1);
      expect(await File(installedTuna()).readAsString(), 'old runner');
      expect(await File(installedMonitor()).readAsString(), 'old monitor');
      expect(
        Directory('${temporary.path}/home/Applications').listSync().length,
        1,
        reason: 'successful rollback cleans staging',
      );
    });

    test(
      'a first-install swap failure restores the absence of both apps',
      () async {
        await checkedApps();
        var checks = 0;
        shell.onCapture = (call) {
          if (call.first == 'pgrep' && ++checks == 4) {
            final parent = Directory('${temporary.path}/home/Applications');
            final staging = parent.listSync().whereType<Directory>().single;
            Directory('${staging.path}/Tuna Monitor.app')
                .deleteSync(recursive: true);
          }
        };
        expect(await dev(['install', 'all']), 1);
        expect(await File(installedTuna()).exists(), isFalse);
        expect(await File(installedMonitor()).exists(), isFalse);
      },
    );

    test(
      'bad native format, permissions and process inspection fail closed',
      () async {
        await checkedApps();
        shell.invalidNative = true;
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Mach-O'));
        shell.invalidNative = false;
        await const ProcessShell().run('chmod', [
          '644',
          '$root/build/tuna/tuna',
        ]);
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Executable permissions required'));
        await const ProcessShell().run('chmod', [
          '755',
          '$root/build/tuna/tuna',
        ]);
        shell.probeExitCode = 2;
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Cannot determine'));
        expect(await File(installedTuna()).exists(), isFalse);
      },
    );

    test(
      'incompatible receipts and interrupted-install backups are refused',
      () async {
        await checkedApps();
        final receipt = File('$root/build/validated-apps.json');
        final valid = await receipt.readAsString();
        await receipt.writeAsString(valid.replaceFirst('macos', 'linux'));
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('incompatible validation receipt'));
        await receipt.writeAsString(valid);
        await Directory(
          '${temporary.path}/home/.local/bin/.tuna-install-recovery',
        ).create(recursive: true);
        expect(await dev(['install', 'all']), 1);
        expect('$err', contains('Recovery artifacts found'));
        expect(
          await Directory(
            '${temporary.path}/home/.local/bin/.tuna-install-recovery',
          ).exists(),
          isTrue,
        );
        expect(await File(installedTuna()).exists(), isFalse);
      },
    );

    test('staged signature failure preserves the installed pair', () async {
      await checkedApps();
      expect(await dev(['install', 'all']), 0);
      await File(installedTuna()).writeAsString('old runner');
      await File(installedMonitor()).writeAsString('old monitor');
      var checks = 0;
      shell.onCapture = (call) {
        if (call.first == 'file' && ++checks == 4) {
          shell.failing.add('codesign --verify');
        }
      };
      expect(await dev(['install', 'all']), 1);
      expect(await File(installedTuna()).readAsString(), 'old runner');
      expect(await File(installedMonitor()).readAsString(), 'old monitor');
    });

    test('a blocked monitor destination leaves the runner unchanged', () async {
      await checkedApps();
      await File(installedTuna()).create(recursive: true);
      await File(installedTuna()).writeAsString('old runner');
      final applications = '${temporary.path}/home/Applications';
      await Directory(applications).create();
      await File('$applications/Tuna Monitor.app')
          .writeAsString('not a bundle');
      expect(await dev(['install', 'all']), 1);
      expect(await File(installedTuna()).readAsString(), 'old runner');
      expect(
        await File('$applications/Tuna Monitor.app').readAsString(),
        'not a bundle',
      );
    });

    test('libraries and unsupported hosts are reported explicitly', () async {
      expect(
        await dev(['install', 'tuna-contract-swift']),
        unavailableExitCode,
      );
      expect('$err', contains('library'));
      expect(await dev(['install', 'all'], os: 'linux'), unavailableExitCode);
      expect('$err', contains('No supported applications'));
      expect(await dev(['install', 'monitor-linux']), unavailableExitCode);
      expect('$err', contains('not implemented'));
      expect(shell.calls, isEmpty);
    });

    test('install all refuses to omit a supported application', () async {
      await checkedApps();
      apps.add(const ExtraMacApp());
      try {
        expect(await dev(['install', 'all']), unavailableExitCode);
        expect('$err', contains('extra-mac-app'));
        expect(await File(installedTuna()).exists(), isFalse);
      } finally {
        apps.removeLast();
      }
    });
  });
}
