// Repository build orchestration: explicit per-application build, test and
// check targets. Uses only `dart:` libraries so `tool/dev.dart`
// runs without `pub get`.
import 'dart:convert';
import 'dart:io';

part 'install.dart';

const usageExitCode = 64;
const unavailableExitCode = 69;

const _usage = '''
Usage: dart tool/dev.dart <command> [arguments]

Commands:
  list                         Show applications and their host support.
  build <app|all>              Compile into build/<app>/. Does not run tests.
  test <app|all>               Run the application's tests.
  check <app|all>              Build, then test; reports both results.
  install all                 Install the unchanged artifacts from check all.

`all` selects the applications implemented for this host.''';

/// Runs external commands. Abstracted so tests can observe the sequence of
/// commands without compiling anything.
abstract class Shell {
  Future<ProcessResult> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  });

  /// Runs a command with inherited output and returns its exit code.
  Future<int> run(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
    Map<String, String>? environment,
  });
}

class ProcessShell implements Shell {
  const ProcessShell();

  @override
  Future<ProcessResult> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  }) => Process.run(executable, arguments, workingDirectory: workingDirectory);

  @override
  Future<int> run(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
    Map<String, String>? environment,
  }) async {
    try {
      final process = await Process.start(
        executable,
        arguments,
        workingDirectory: workingDirectory,
        environment: environment,
        mode: ProcessStartMode.inheritStdio,
      );
      return await process.exitCode;
    } on ProcessException catch (error) {
      stderr.writeln('Cannot run $executable: ${error.message}');
      return 127;
    }
  }
}

/// Where and how the orchestrator runs.
class DevContext {
  DevContext({
    required this.root,
    required this.os,
    required this.dart,
    this.home,
    this.shell = const ProcessShell(),
    StringSink? out,
    StringSink? err,
  }) : this.out = out ?? stdout,
       this.err = err ?? stderr;

  factory DevContext.host(String root) => DevContext(
    root: root,
    os: Platform.operatingSystem,
    dart: Platform.resolvedExecutable,
    home: Platform.environment['HOME'],
  );

  final String root;
  final String os;
  final String dart;
  final String? home;
  final Shell shell;
  final StringSink out;
  final StringSink err;

  String get buildRoot => '${this.root}/build';
}

enum Support { implemented, future }

/// One shipped application. Platform-specific details live in each adapter.
abstract class App {
  const App();

  String get name;
  String get description;
  Support get support => Support.implemented;
  bool get installable => true;

  /// Hosts on which this application can be built and tested.
  Set<String> get hosts;

  Future<int> build(DevContext context);
  Future<int> test(DevContext context);
}

final List<App> apps = [
  const TunaApp(),
  const SwiftContractApp(),
  const TunaMonitorApp(),
  const FutureApp('monitor-windows', 'Windows status monitor', 'windows'),
  const FutureApp('monitor-linux', 'Linux status monitor', 'linux'),
];

Future<int> runDev(List<String> arguments, DevContext context) async {
  if (arguments.isEmpty) {
    context.err.writeln(_usage);
    return usageExitCode;
  }
  final command = arguments.first;
  final rest = arguments.sublist(1);
  switch (command) {
    case '-h' || '--help' || 'help' when rest.isEmpty:
      context.out.writeln(_usage);
      return 0;
    case 'list' when rest.isEmpty:
      _list(context);
      return 0;
    case 'build' || 'test' || 'check' when rest.length == 1:
      return _withArtifactsLock(
        context,
        () => _forTargets(context, command, rest.single),
      );
    case 'install' when rest.length == 1:
      return _withArtifactsLock(context, () => _install(context, rest.single));
  }
  context.err.writeln(_usage);
  return usageExitCode;
}

Future<int> _withArtifactsLock(
  DevContext context,
  Future<int> Function() action,
) async {
  RandomAccessFile? lock;
  try {
    await Directory(context.buildRoot).create(recursive: true);
    lock = await File('${context.buildRoot}/.dev.lock')
        .open(mode: FileMode.append);
    await lock.lock(FileLock.exclusive);
    return await action();
  } on FileSystemException catch (error) {
    context.err.writeln(
      'Orchestrator failed: ${error.message}. '
      'Another command may be active; do not remove its lock file.',
    );
    return 1;
  } finally {
    await lock?.close();
  }
}

void _list(DevContext context) {
  for (final app in apps) {
    final status = switch (app.support) {
      Support.future => 'future (not implemented)',
      Support.implemented when app.hosts.contains(context.os) => 'implemented',
      Support.implemented =>
        'implemented, unsupported on ${context.os} '
            '(${app.hosts.join(', ')} only)',
    };
    context.out.writeln(
      '${app.name.padRight(16)} $status - ${app.description}',
    );
  }
}

Future<int> _forTargets(
  DevContext context,
  String command,
  String target,
) async {
  final List<App> selected;
  if (target == 'all') {
    selected = [
      for (final app in apps)
        if (app.support == Support.implemented &&
            app.hosts.contains(context.os))
          app,
    ];
    for (final app in apps.where((app) => !selected.contains(app))) {
      context.out.writeln(
        'Skipping ${app.name}: ${app.support == Support.future ? 'not implemented' : 'unsupported on ${context.os}'}.',
      );
    }
    if (selected.isEmpty) {
      context.err.writeln('No supported applications on ${context.os}.');
      return unavailableExitCode;
    }
  } else {
    final matches = apps.where((app) => app.name == target);
    if (matches.isEmpty) {
      context.err.writeln(
        'Unknown application: $target. '
        'Known: ${apps.map((app) => app.name).join(', ')}, all.',
      );
      return usageExitCode;
    }
    final app = matches.single;
    if (app.support == Support.future) {
      context.err.writeln('${app.name} is not implemented yet.');
      return unavailableExitCode;
    }
    if (!app.hosts.contains(context.os)) {
      context.err.writeln(
        '${app.name} is not supported on ${context.os} '
        '(${app.hosts.join(', ')} only).',
      );
      return unavailableExitCode;
    }
    selected = [app];
  }

  // A failed or partial recheck must not authorize installation of old output.
  final receipt = File('${context.buildRoot}/validated-apps.json');
  if (await receipt.exists()) await receipt.delete();
  final failures = <String>[];
  for (final app in selected) {
    final steps = switch (command) {
      'build' => ['build'],
      'test' => ['test'],
      _ => ['build', 'test'],
    };
    for (final step in steps) {
      context.out.writeln('==> $step ${app.name}');
      final code = switch (step) {
        'build' => await app.build(context),
        _ => await app.test(context),
      };
      if (code != 0) failures.add('$step ${app.name} (exit $code)');
    }
  }
  if (failures.isNotEmpty) {
    context.err.writeln('Failed: ${failures.join(', ')}');
    return 1;
  }
  if (command == 'check' && target == 'all') {
    if (!_checkInstallTargets(context)) return unavailableExitCode;
    try {
      await _validateArtifacts(context, context.buildRoot);
      final checksums = await _checksums(context, context.buildRoot);
      await receipt.writeAsString(
        jsonEncode({'os': context.os, 'checksums': checksums}),
        flush: true,
      );
      context.out.writeln('Validated app set: ready for install all.');
    } on FileSystemException catch (error) {
      context.err.writeln('Cannot validate app set: ${error.message}');
      return 1;
    } on ProcessException catch (error) {
      context.err.writeln('Cannot validate app set: ${error.message}');
      return 1;
    }
  }
  return 0;
}

class FutureApp extends App {
  const FutureApp(this.name, this.description, String host) : this._host = host;

  @override
  final String name;
  @override
  final String description;
  final String _host;

  @override
  Support get support => Support.future;
  @override
  Set<String> get hosts => {this._host};

  @override
  Future<int> build(DevContext context) async => unavailableExitCode;
  @override
  Future<int> test(DevContext context) async => unavailableExitCode;
}

/// The Tuna runner/broker executable.
class TunaApp extends App {
  const TunaApp();

  @override
  String get name => 'tuna';
  @override
  String get description => 'Tuna runner and broker (Go)';
  @override
  Set<String> get hosts => const {'macos'};

  /// Broker for integration tests outside the Go test binary, such as the
  /// monitor's. Its build tag requires `TUNA_TEST_LOCK_DIR`, so it never
  /// takes the account's real singleton lock; the production executable
  /// has no such override.
  static const testBroker = 'tuna-test-broker';
  static const testBrokerTag = 'tuna_test_broker';

  String _package(DevContext context) => '${context.root}/apps/tuna';

  @override
  Future<int> build(DevContext context) async {
    final output = Directory('${context.buildRoot}/tuna');
    await output.create(recursive: true);
    final executables = <String, List<String>>{
      'tuna': const [],
      testBroker: const ['-tags', testBrokerTag],
    };
    final staged = <String, String>{};
    try {
      for (final MapEntry(key: executable, value: flags)
          in executables.entries) {
        final temporary = '${output.path}/.$executable.$pid.tmp';
        staged[executable] = temporary;
        final code = await context.shell.run('go', [
          'build',
          '-trimpath',
          '-o',
          temporary,
          ...flags,
          '.',
        ], workingDirectory: this._package(context));
        if (code != 0 || !await File(temporary).exists()) {
          context.err.writeln(
            'Build failed; existing executables were not replaced.',
          );
          return code == 0 ? 1 : code;
        }
      }
      for (final MapEntry(key: executable, value: temporary)
          in staged.entries) {
        await File(temporary).rename('${output.path}/$executable');
      }
    } finally {
      for (final temporary in staged.values) {
        final file = File(temporary);
        if (await file.exists()) await file.delete();
      }
    }
    for (final executable in executables.keys) {
      context.out.writeln('Built: ${output.path}/$executable');
    }
    return 0;
  }

  @override
  Future<int> test(DevContext context) async {
    final code = await context.shell.run('go', [
      'test',
      './...',
    ], workingDirectory: this._package(context));
    if (code != 0) return code;
    return context.shell.run(
      'python3',
      [
        '${context.root}/docs/model/check.py',
        'DescendantScanSafety',
        'DescendantScanNoBarrier',
        'DescendantScanUnstable',
        'DescendantScanStable',
        'DescendantScanRestart',
        'DescendantScanFailure',
        'RunnerFinishSafety',
        'RunnerFinishBlocking',
        'RunnerFinishDropped',
        'RunnerFinishEarlyAck',
        'RunnerFinishEarlyFinal',
        'RunnerExitSafety',
        'RunnerExitFailureSafety',
        'RunnerExitOldRule',
        'RunnerExitNoBarrier',
        'RunnerExitCleanRelease',
        'RunnerExitDeathTakeover',
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
        '45',
        '--logs',
        '${context.buildRoot}/tuna-model',
      ],
      workingDirectory: context.root,
      environment: {'JAVA_TOOL_OPTIONS': '-Xmx256m'},
    );
  }
}

/// Generated Swift bindings for the Tuna contract, consumed by the monitor.
class SwiftContractApp extends App {
  const SwiftContractApp();

  @override
  String get name => 'tuna-contract-swift';
  @override
  String get description => 'Tuna contract Swift bindings (library)';
  @override
  bool get installable => false;
  @override
  Set<String> get hosts => const {'macos'};

  String _package(DevContext context) =>
      '${context.root}/packages/tuna-contract-swift';

  @override
  Future<int> build(DevContext context) => context.shell.run('swift', [
    'build',
    resolvedOnly,
    '--package-path',
    this._package(context),
  ]);

  @override
  Future<int> test(DevContext context) => context.shell.run('swift', [
    'test',
    resolvedOnly,
    '--package-path',
    this._package(context),
  ]);

  /// Builds only the dependency versions in Package.resolved, which flake.nix
  /// verifies against its Swift pins.
  static const resolvedOnly = '--only-use-versions-from-resolved-file';
}

/// The Tuna macOS monitor.
class TunaMonitorApp extends App {
  const TunaMonitorApp();

  static const bundleName = 'Tuna Monitor.app';

  @override
  String get name => 'tuna-monitor-macos';
  @override
  String get description => 'Tuna macOS monitor and controls';
  @override
  Set<String> get hosts => const {'macos'};

  String _package(DevContext context) =>
      '${context.root}/apps/tuna-monitor-macos';

  @override
  Future<int> build(DevContext context) => buildMacBundle(
    context,
    package: this._package(context),
    executable: 'TunaMonitor',
    bundle: '${context.buildRoot}/tuna-monitor-macos/$bundleName',
    swiftArguments: const [SwiftContractApp.resolvedOnly],
  );

  /// Runs the integration test against the isolated test broker when it has
  /// been built (`check all` builds tuna first); it is skipped otherwise.
  @override
  Future<int> test(DevContext context) async {
    final tuna = File('${context.buildRoot}/tuna/${TunaApp.testBroker}');
    final integration = await tuna.exists();
    if (!integration) {
      context.out.writeln(
        'Skipping the broker integration test: ${tuna.path} is not built.',
      );
    }
    return context.shell.run('swift', [
      'test',
      SwiftContractApp.resolvedOnly,
      '--package-path',
      this._package(context),
    ], environment: integration ? {'TUNA_MONITOR_TEST_TUNA': tuna.path} : null);
  }
}

/// Builds [executable] from the Swift [package] in release mode and replaces
/// the signed app [bundle], keeping the previous bundle on failure.
Future<int> buildMacBundle(
  DevContext context, {
  required String package,
  required String executable,
  required String bundle,
  List<String> swiftArguments = const [],
}) async {
  var code = await context.shell.run('swift', [
    'build',
    ...swiftArguments,
    '--package-path',
    package,
    '-c',
    'release',
  ]);
  if (code != 0) return code;
  final binary = File('$package/.build/release/$executable');
  if (!await binary.exists()) {
    context.err.writeln('Swift build produced no executable: ${binary.path}');
    return 1;
  }
  final output = Directory(File(bundle).parent.path);
  await output.create(recursive: true);
  final staging = await output.createTemp('.staging-');
  try {
    final staged = '${staging.path}/${bundle.split('/').last}';
    await Directory('$staged/Contents/MacOS').create(recursive: true);
    await binary.copy('$staged/Contents/MacOS/$executable');
    await File('$package/Info.plist').copy('$staged/Contents/Info.plist');
    code = await context.shell.run('chmod', [
      '755',
      '$staged/Contents/MacOS/$executable',
    ]);
    if (code == 0) {
      code = await context.shell.run('codesign', [
        '--force',
        '--sign',
        '-',
        staged,
      ]);
    }
    if (code != 0) {
      context.err.writeln('Bundle signing failed; previous build kept.');
      return code;
    }
    final result = await replaceDirectory(staged, bundle);
    if (result != null) {
      context.err.writeln(result);
      return 1;
    }
  } finally {
    if (await staging.exists()) await staging.delete(recursive: true);
  }
  context.out.writeln('Built: $bundle');
  return 0;
}

/// Moves [source] to [target], replacing an existing directory so a failure
/// leaves either the old or the new copy in place. Returns an error message
/// on failure.
Future<String?> replaceDirectory(String source, String target) async {
  final existing = Directory(target);
  final backup = '$target.previous-$pid';
  final hadExisting = await existing.exists();
  if (hadExisting) {
    try {
      await existing.rename(backup);
    } on FileSystemException catch (error) {
      return 'Cannot replace $target: ${error.message}; it was not changed.';
    }
  }
  try {
    await Directory(source).rename(target);
  } on FileSystemException catch (error) {
    if (hadExisting) await Directory(backup).rename(target);
    return 'Cannot replace $target: ${error.message}; it was not changed.';
  }
  if (hadExisting) await Directory(backup).delete(recursive: true);
  return null;
}
