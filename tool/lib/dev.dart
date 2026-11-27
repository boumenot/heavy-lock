// Repository build orchestration: explicit per-application build, test,
// check and install targets. Uses only `dart:` libraries so `tool/dev.dart`
// runs without `pub get`.
import 'dart:io';

const usageExitCode = 64;
const unavailableExitCode = 69;

const _usage = '''
Usage: dart tool/dev.dart <command> [arguments]

Commands:
  list                         Show applications and their host support.
  version                      Print the heavy-lock version stamp.
  build <app|all>              Compile into build/<app>/. Does not run tests.
  test <app|all>               Run the application's tests.
  check <app|all>              Build, then test; reports both results.
  install <app|all> [dir]      Install a built application. A destination
                               directory is only accepted for a single app.
                               `all` skips applications that are not
                               installable.

`all` selects the applications implemented for this host.''';

/// Runs external commands. Abstracted so tests can observe the sequence of
/// commands without compiling anything.
abstract class Shell {
  /// Runs a command with inherited output and returns its exit code.
  Future<int> run(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
    Map<String, String>? environment,
  });

  /// Runs a command and returns its exit code and trimmed standard output.
  Future<(int, String)> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  });
}

class ProcessShell implements Shell {
  const ProcessShell();

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

  @override
  Future<(int, String)> capture(
    String executable,
    List<String> arguments, {
    String? workingDirectory,
  }) async {
    try {
      final result = await Process.run(
        executable,
        arguments,
        workingDirectory: workingDirectory,
      );
      return (result.exitCode, (result.stdout as String).trim());
    } on ProcessException {
      return (127, '');
    }
  }
}

/// Where and how the orchestrator runs.
class DevContext {
  DevContext({
    required this.root,
    required this.os,
    required this.dart,
    required this.home,
    this.shell = const ProcessShell(),
    StringSink? out,
    StringSink? err,
  }) : this.out = out ?? stdout,
       this.err = err ?? stderr;

  factory DevContext.host(String root) => DevContext(
    root: root,
    os: Platform.operatingSystem,
    dart: Platform.resolvedExecutable,
    home: Platform.environment['HOME'] ?? Platform.environment['USERPROFILE'],
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

  /// Whether `install` applies. Prototypes and libraries are built and
  /// tested but never installed.
  bool get installable => true;

  /// Hosts on which this application can be built, tested and installed.
  Set<String> get hosts;

  Future<int> build(DevContext context);
  Future<int> test(DevContext context);
  Future<int> install(DevContext context, String? destination);
}

final List<App> apps = [
  const HeavyLockApp(),
  const MacMonitorApp(),
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
    case 'version' when rest.isEmpty:
      final stamp = await heavyLockVersion(context);
      if (stamp == null) return 1;
      context.out
        ..writeln(stamp.version)
        ..writeln(stamp.description);
      return 0;
    case 'build' || 'test' || 'check' when rest.length == 1:
      return _forTargets(context, command, rest.single, null);
    case 'install' when rest.length == 1 || rest.length == 2:
      if (rest.length == 2 && (rest.first == 'all' || rest.last.isEmpty)) {
        context.err.writeln(_usage);
        return usageExitCode;
      }
      return _forTargets(
        context,
        command,
        rest.first,
        rest.length == 2 ? rest.last : null,
      );
  }
  context.err.writeln(_usage);
  return usageExitCode;
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
  String? destination,
) async {
  final List<App> selected;
  if (target == 'all') {
    selected = [
      for (final app in apps)
        if (app.support == Support.implemented &&
            app.hosts.contains(context.os) &&
            (command != 'install' || app.installable))
          app,
    ];
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
    if (command == 'install' && !app.installable) {
      context.err.writeln('${app.name} is not installable.');
      return unavailableExitCode;
    }
    selected = [app];
  }

  final failures = <String>[];
  for (final app in selected) {
    final steps = switch (command) {
      'build' => ['build'],
      'test' => ['test'],
      'check' => ['build', 'test'],
      _ => ['install'],
    };
    for (final step in steps) {
      context.out.writeln('==> $step ${app.name}');
      final code = switch (step) {
        'build' => await app.build(context),
        'test' => await app.test(context),
        _ => await app.install(context, destination),
      };
      if (code != 0) failures.add('$step ${app.name} (exit $code)');
    }
  }
  if (failures.isNotEmpty) {
    context.err.writeln('Failed: ${failures.join(', ')}');
    return 1;
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
  @override
  Future<int> install(DevContext context, String? destination) async =>
      unavailableExitCode;
}

class HeavyLockApp extends App {
  const HeavyLockApp();

  @override
  String get name => 'heavy-lock';
  @override
  String get description => 'Dart admission runner and status CLI';
  @override
  Set<String> get hosts => const {'macos', 'linux', 'windows'};

  String _package(DevContext context) => '${context.root}/apps/heavy-lock';

  @override
  Future<int> build(DevContext context) async {
    final stamp = await heavyLockVersion(context);
    if (stamp == null) {
      context.err.writeln(
        'Cannot determine the version; existing executables were not '
        'replaced.',
      );
      return 1;
    }
    final output = Directory('${context.buildRoot}/heavy-lock');
    // Earlier builds wrote the runner directly to build/heavy-lock.
    final legacy = File(output.path);
    if (await legacy.exists()) await legacy.delete();
    await output.create(recursive: true);
    final suffix = context.os == 'windows' ? '.exe' : '';
    final executables = {
      'heavy-lock$suffix': 'bin/heavy_lock.dart',
      'heavy-lock-status$suffix': 'bin/heavy_lock_status.dart',
    };
    final staged = <String, String>{};
    try {
      for (final MapEntry(key: executable, value: source)
          in executables.entries) {
        final temporary = '${output.path}/.$executable.$pid.tmp';
        staged[executable] = temporary;
        final code = await context.shell.run(context.dart, [
          'compile',
          'exe',
          '-Dheavy_lock.version=${stamp.version}',
          '-Dheavy_lock.build=${stamp.description}',
          source,
          '-o',
          temporary,
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
    context.out.writeln(
      'Built: ${output.path}/heavy-lock$suffix '
      '(${stamp.version}, ${stamp.description})',
    );
    return 0;
  }

  @override
  Future<int> test(DevContext context) => context.shell.run(context.dart, [
    'test',
  ], workingDirectory: this._package(context));

  @override
  Future<int> install(DevContext context, String? destination) async {
    if (context.os == 'windows') {
      context.err.writeln(
        'Installing heavy-lock is not automated on Windows yet; copy '
        r'build\heavy-lock\heavy-lock.exe to a directory on PATH.',
      );
      return unavailableExitCode;
    }
    return context.shell.run('bash', [
      '${context.root}/tool/install.sh',
      if (destination != null) destination,
    ]);
  }
}

class MacMonitorApp extends App {
  const MacMonitorApp();

  static const bundleName = 'Heavy Lock Status.app';

  @override
  String get name => 'monitor-macos';
  @override
  String get description => 'macOS Dock and menu status monitor';
  @override
  Set<String> get hosts => const {'macos'};

  String _package(DevContext context) => '${context.root}/apps/monitor-macos';
  String _bundle(DevContext context) =>
      '${context.buildRoot}/monitor-macos/$bundleName';

  @override
  Future<int> build(DevContext context) => buildMacBundle(
    context,
    package: this._package(context),
    executable: 'HeavyLockMonitor',
    bundle: this._bundle(context),
  );

  @override
  Future<int> test(DevContext context) => context.shell.run(
    'swift',
    ['test', '--package-path', this._package(context)],
    environment: {'HEAVY_LOCK_MONITOR_DART': context.dart},
  );

  @override
  Future<int> install(DevContext context, String? destination) async {
    final built = this._bundle(context);
    if (!await File('$built/Contents/MacOS/HeavyLockMonitor').exists()) {
      context.err.writeln(
        'Built app not found: $built; run '
        '`dart tool/dev.dart build monitor-macos` first.',
      );
      return 1;
    }
    final home = context.home;
    if (destination == null && home == null) {
      context.err.writeln('HOME must be set.');
      return 1;
    }
    final directory = Directory(destination ?? '$home/Applications');
    final target = '${directory.path}/$bundleName';
    if (await FileSystemEntity.type(target, followLinks: false) case final type
        when type != FileSystemEntityType.notFound &&
            type != FileSystemEntityType.directory) {
      context.err.writeln('Install destination is not an app bundle: $target');
      return 1;
    }
    await directory.create(recursive: true);
    final staging = await directory.createTemp('.heavy-lock-monitor-');
    try {
      final copy = '${staging.path}/$bundleName';
      // ditto preserves the bundle's code signature and metadata.
      final code = await context.shell.run('ditto', [built, copy]);
      if (code != 0 ||
          !await File('$copy/Contents/MacOS/HeavyLockMonitor').exists()) {
        context.err.writeln(
          'Installation failed; existing app was not replaced.',
        );
        return code == 0 ? 1 : code;
      }
      final result = await replaceDirectory(copy, target);
      if (result != null) {
        context.err.writeln(result);
        return 1;
      }
    } finally {
      if (await staging.exists()) await staging.delete(recursive: true);
    }
    context.out.writeln(
      'Installed: $target (quit and reopen a running monitor to upgrade)',
    );
    return 0;
  }
}

/// The v2 runner/broker executable. Until the v2 cutover it is a prototype
/// that refuses to run work, so it is never installed.
class TunaApp extends App {
  const TunaApp();

  @override
  String get name => 'tuna';
  @override
  String get description => 'v2 runner/broker prototype (Go; runs no work)';
  @override
  Set<String> get hosts => const {'macos'};
  @override
  bool get installable => false;

  String _package(DevContext context) => '${context.root}/apps/tuna';

  @override
  Future<int> build(DevContext context) async {
    final output = Directory('${context.buildRoot}/tuna');
    await output.create(recursive: true);
    final temporary = File('${output.path}/.tuna.$pid.tmp');
    try {
      final code = await context.shell.run('go', [
        'build',
        '-trimpath',
        '-o',
        temporary.path,
        '.',
      ], workingDirectory: this._package(context));
      if (code != 0 || !await temporary.exists()) {
        context.err.writeln(
          'Build failed; existing executable was not replaced.',
        );
        return code == 0 ? 1 : code;
      }
      await temporary.rename('${output.path}/tuna');
    } finally {
      if (await temporary.exists()) await temporary.delete();
    }
    context.out.writeln('Built: ${output.path}/tuna');
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
        '${context.root}/docs/v2/model/check.py',
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
        '--logs',
        '${context.buildRoot}/tuna-model',
      ],
      workingDirectory: context.root,
      environment: {'JAVA_TOOL_OPTIONS': '-Xmx256m'},
    );
  }

  @override
  Future<int> install(DevContext context, String? destination) async =>
      unavailableExitCode;
}

/// Generated Swift bindings for the v2 contract, consumed by the v2
/// monitor. A library, so it is never installed.
class SwiftContractApp extends App {
  const SwiftContractApp();

  @override
  String get name => 'heavylock-v2-swift';
  @override
  String get description => 'v2 contract Swift bindings (library)';
  @override
  Set<String> get hosts => const {'macos'};
  @override
  bool get installable => false;

  String _package(DevContext context) =>
      '${context.root}/packages/heavylock-v2-swift';

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

  @override
  Future<int> install(DevContext context, String? destination) async =>
      unavailableExitCode;

  /// Builds only the dependency versions in Package.resolved, which flake.nix
  /// verifies against its Swift pins.
  static const resolvedOnly = '--only-use-versions-from-resolved-file';
}

/// The v2 macOS monitor. Until the v2 cutover it is built and tested but
/// never installed, so it cannot replace the v1 monitor.
class TunaMonitorApp extends App {
  const TunaMonitorApp();

  static const bundleName = 'Tuna Monitor.app';

  @override
  String get name => 'tuna-monitor-macos';
  @override
  String get description => 'v2 macOS monitor and controls (prototype)';
  @override
  Set<String> get hosts => const {'macos'};
  @override
  bool get installable => false;

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

  /// Runs the integration test against `build/tuna/tuna` when it has been
  /// built (`check all` builds tuna first); it is skipped otherwise.
  @override
  Future<int> test(DevContext context) async {
    final tuna = File('${context.buildRoot}/tuna/tuna');
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

  @override
  Future<int> install(DevContext context, String? destination) async =>
      unavailableExitCode;
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

class VersionStamp {
  const VersionStamp(this.version, this.description);

  final String version;
  final String description;
}

/// Derives the runner's version from Git:
///
///     <major>.<minor>.<commit count>+<hash>        master build
///     <major>.<minor>.<commit count>-dirty+<hash>  anything else
///
/// A master build is a clean checkout whose HEAD is on `master` or
/// `origin/master`. Major and minor come from the runner's pubspec; the
/// patch counts commits reachable from HEAD, so every commit bumps it.
Future<VersionStamp?> heavyLockVersion(DevContext context) async {
  final pubspec = File('${context.root}/apps/heavy-lock/pubspec.yaml');
  final match = await pubspec.exists()
      ? RegExp(
          r'^version:\s*(\d+\.\d+)\.\d+',
          multiLine: true,
        ).firstMatch(await pubspec.readAsString())
      : null;
  if (match == null) {
    context.err.writeln('Cannot read <major>.<minor> from ${pubspec.path}');
    return null;
  }
  final base = match.group(1)!;

  Future<(int, String)> git(List<String> arguments) =>
      context.shell.capture('git', ['-C', context.root, ...arguments]);

  if ((await git(['rev-parse', '--verify', '-q', 'HEAD^{commit}'])).$1 != 0) {
    return VersionStamp(
      '$base.0-dirty+unknown',
      'non-master build: not a git checkout',
    );
  }
  final (countCode, count) = await git(['rev-list', '--count', 'HEAD']);
  final (hashCode, hash) = await git(['rev-parse', '--short=12', 'HEAD']);
  final (statusCode, status) = await git(['status', '--porcelain']);
  if (countCode != 0 || hashCode != 0 || statusCode != 0) {
    context.err.writeln('Cannot read Git metadata in ${context.root}');
    return null;
  }

  final reasons = <String>[];
  var hasMaster = false;
  var onMaster = false;
  for (final ref in ['master', 'origin/master']) {
    if ((await git(['rev-parse', '--verify', '-q', '$ref^{commit}'])).$1 == 0) {
      hasMaster = true;
      if ((await git(['merge-base', '--is-ancestor', 'HEAD', ref])).$1 == 0) {
        onMaster = true;
      }
    }
  }
  if (!hasMaster) {
    reasons.add('no master branch');
  } else if (!onMaster) {
    final (code, branch) = await git(['symbolic-ref', '--short', '-q', 'HEAD']);
    reasons.add(
      'commit not on master (branch ${code == 0 ? branch : 'detached HEAD'})',
    );
  }
  if (status.isNotEmpty) reasons.add('uncommitted changes');

  return reasons.isEmpty
      ? VersionStamp('$base.$count+$hash', 'master build')
      : VersionStamp(
          '$base.$count-dirty+$hash',
          'non-master build: ${reasons.join('; ')}',
        );
}
