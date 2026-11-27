part of 'dev.dart';

const _monitorArtifact = 'tuna-monitor-macos/${TunaMonitorApp.bundleName}';

bool _checkInstallTargets(DevContext context) {
  const handled = {'tuna', 'tuna-monitor-macos'};
  final supported = {
    for (final app in apps)
      if (app.support == Support.implemented &&
          app.hosts.contains(context.os) &&
          app.installable)
        app.name,
  };
  final unhandled = supported.difference(handled);
  final missing = handled.difference(supported);
  if (unhandled.isEmpty && missing.isEmpty) return true;
  context.err.writeln(
    'Install target configuration does not match supported apps on '
    '${context.os}; unhandled: ${unhandled.join(', ')}; '
    'unavailable: ${missing.join(', ')}.',
  );
  return false;
}

Future<String> _checksums(
  DevContext context,
  String buildRoot, {
  String? tuna,
  String? monitor,
}) async {
  final bundle = monitor ?? '$buildRoot/$_monitorArtifact';
  final paths = {'tuna/tuna': tuna ?? '$buildRoot/tuna/tuna'};
  await for (final entry in Directory(
    bundle,
  ).list(recursive: true, followLinks: false)) {
    if (entry is Link) {
      throw FileSystemException('Symlink in app artifact: ${entry.path}');
    }
    if (entry is File) {
      if ((await entry.stat()).type != FileSystemEntityType.file) {
        throw FileSystemException('Non-regular app artifact: ${entry.path}');
      }
      paths['$_monitorArtifact/${entry.path.substring(bundle.length + 1)}'] =
          entry.path;
    }
  }
  final names = paths.keys.toList()..sort();
  final checksums = StringBuffer();
  for (final name in names) {
    final result = await context.shell.capture('/usr/bin/shasum', [
      '-a',
      '256',
      paths[name]!,
    ]);
    final hash = RegExp(r'^[0-9a-f]{64}').firstMatch(result.stdout as String);
    if (result.exitCode != 0 || hash == null) {
      throw FileSystemException('Artifact hashing failed: ${result.stderr}');
    }
    checksums.writeln('${hash.group(0)}  $name');
  }
  return checksums.toString();
}

Future<void> _validateArtifacts(
  DevContext context,
  String buildRoot, {
  String? tuna,
  String? monitor,
}) async {
  final bundle = monitor ?? '$buildRoot/$_monitorArtifact';
  if (await FileSystemEntity.type(bundle, followLinks: false) !=
      FileSystemEntityType.directory) {
    throw FileSystemException('Missing regular app bundle: $bundle');
  }
  for (final binary in [
    tuna ?? '$buildRoot/tuna/tuna',
    '$bundle/Contents/MacOS/TunaMonitor',
  ]) {
    if (await FileSystemEntity.type(binary, followLinks: false) !=
        FileSystemEntityType.file) {
      throw FileSystemException('Missing regular executable: $binary');
    }
    if ((await File(binary).stat()).mode & 0x49 != 0x49) {
      throw FileSystemException('Executable permissions required: $binary');
    }
    final result = await context.shell.capture('/usr/bin/file', ['-b', binary]);
    if (result.exitCode != 0 ||
        !(result.stdout as String).contains('Mach-O') ||
        !(result.stdout as String).contains('arm64')) {
      throw FileSystemException(
        'Expected an Apple Silicon Mach-O executable: $binary (${result.stdout}; ${result.stderr})',
      );
    }
  }
  for (final command in [
    ['/usr/bin/plutil', '-lint', '$bundle/Contents/Info.plist'],
    ['/usr/bin/codesign', '--verify', '--deep', '--strict', bundle],
  ]) {
    final code = await context.shell.run(command.first, command.sublist(1));
    if (code != 0) {
      throw FileSystemException(
        'Artifact validation failed (exit $code): ${command.join(' ')}',
      );
    }
  }
}

Future<void> _requireStopped(DevContext context) async {
  for (final name in ['tuna', 'TunaMonitor']) {
    final result = await context.shell.capture('/usr/bin/pgrep', ['-x', name]);
    if (result.exitCode == 0) {
      throw FileSystemException(
        '$name is running. Drain all queued/running jobs, then separately '
        'stop the broker agent and quit the monitor; keep both stopped '
        'until installation finishes. No process or service was changed.',
      );
    }
    if (result.exitCode != 1) {
      throw FileSystemException(
        'Cannot determine whether $name is running: ${result.stderr}',
      );
    }
  }
}

Future<int> _install(DevContext context, String target) async {
  if (target != 'all') {
    final matches = apps.where((app) => app.name == target);
    if (matches.isEmpty) {
      context.err.writeln('Unknown application: $target.');
      return usageExitCode;
    }
    final app = matches.single;
    if (app.support == Support.future || !app.hosts.contains(context.os)) {
      context.err.writeln(
        '${app.name} is ${app.support == Support.future ? 'not implemented' : 'unsupported on ${context.os}'}.',
      );
      return unavailableExitCode;
    }
    context.err.writeln(
      target == 'tuna-contract-swift'
          ? '$target is a non-installable library.'
          : 'Use install all to keep matching Tuna and monitor artifacts together.',
    );
    return unavailableExitCode;
  }
  if (context.os != 'macos') {
    context.err.writeln('No supported applications on ${context.os}.');
    return unavailableExitCode;
  }
  if (!_checkInstallTargets(context)) return unavailableExitCode;
  final receipt = File('${context.buildRoot}/validated-apps.json');
  if (!await receipt.exists()) {
    context.err.writeln(
      'No validated app set. Run the brokered check all command first.',
    );
    return unavailableExitCode;
  }
  final home = context.home;
  if (home == null || home.isEmpty || !home.startsWith('/')) {
    context.err.writeln('Installation requires an absolute account HOME.');
    return unavailableExitCode;
  }

  final replacements = <_Replacement>[];
  RandomAccessFile? lock;
  var safeToClean = true;
  var result = 1;
  try {
    final manifest = jsonDecode(await receipt.readAsString());
    if (manifest is! Map<String, dynamic> ||
        manifest['os'] != context.os ||
        manifest['checksums'] is! String) {
      throw const FormatException('Invalid or incompatible validation receipt');
    }
    final expected = manifest['checksums'] as String;
    await _validateArtifacts(context, context.buildRoot);
    if (await _checksums(context, context.buildRoot) != expected) {
      throw const FormatException('Artifacts changed after check all');
    }
    await _requireStopped(context);
    final bin = Directory('$home/.local/bin');
    await bin.create(recursive: true);
    lock = await File('${bin.path}/.tuna-install.lock')
        .open(mode: FileMode.append);
    await lock.lock(FileLock.exclusive);
    for (final item in [
      ('tuna/tuna', '${bin.path}/tuna', false),
      (
        _monitorArtifact,
        '$home/Applications/${TunaMonitorApp.bundleName}',
        true,
      ),
    ]) {
      final replacement = _Replacement(item.$2, item.$3);
      replacements.add(replacement);
      await replacement.stage('${context.buildRoot}/${item.$1}');
    }
    // Verify the copies, not just the source, before touching either installed app.
    await _validateArtifacts(
      context,
      context.buildRoot,
      tuna: replacements[0].staged,
      monitor: replacements[1].staged,
    );
    if (await _checksums(
          context,
          context.buildRoot,
          tuna: replacements[0].staged,
          monitor: replacements[1].staged,
        ) !=
        expected) {
      throw const FormatException('Staged artifacts do not match check all');
    }
    await _requireStopped(context);
    safeToClean = false;
    try {
      for (final replacement in replacements) {
        await replacement.swap();
      }
    } on FileSystemException {
      var restored = true;
      for (final replacement in replacements.reversed) {
        try {
          await replacement.restore();
        } on FileSystemException catch (error) {
          restored = false;
          context.err.writeln(
            'Cannot restore ${replacement.target}: ${error.message}',
          );
        }
      }
      safeToClean = restored;
      rethrow;
    }
    safeToClean = true;
    context.out.writeln(
      'Installed Tuna and Tuna Monitor. No services, processes, data or login '
      'settings changed. Separately restart the broker using ${bin.path}/tuna, '
      'verify readiness on the configured port, then open '
      '$home/Applications/${TunaMonitorApp.bundleName}. '
      'Existing agents pointing elsewhere require explicit reconfiguration.',
    );
    context.out.writeln(
      'Skipping tuna-contract-swift: non-installable library; '
      'monitor-windows and monitor-linux: not implemented.',
    );
    result = 0;
  } on FileSystemException catch (error) {
    context.err.writeln('Installation failed: ${error.message}');
  } on FormatException catch (error) {
    context.err.writeln('${error.message}. Rerun brokered check all.');
  } on ProcessException catch (error) {
    context.err.writeln('Installation prerequisite failed: ${error.message}');
  } finally {
    if (safeToClean) {
      for (final replacement in replacements) {
        try {
          await replacement.clean();
        } on FileSystemException catch (error) {
          result = 1;
          context.err.writeln(
            'Cannot clean installation staging at ${replacement.staging?.path}: '
            '${error.message}. Inspect before retrying.',
          );
        }
      }
    } else {
      for (final replacement in replacements) {
        context.err.writeln(
          'Replacement/rollback interrupted; keep both apps stopped. '
          'Recovery artifacts retained at ${replacement.staging?.path}; '
          'destination ${replacement.target}. Inspect before retrying.',
        );
      }
    }
    try {
      await lock?.close();
    } on FileSystemException catch (error) {
      result = 1;
      context.err.writeln('Cannot close installer lock: ${error.message}');
    }
  }
  return result;
}

Future<void> _copyDirectory(String source, String target) async {
  await Directory(target).create();
  await for (final entry in Directory(source).list(followLinks: false)) {
    final destination = '$target/${entry.path.split('/').last}';
    if (entry is File) {
      await entry.copy(destination);
    } else if (entry is Directory) {
      await _copyDirectory(entry.path, destination);
    } else {
      throw FileSystemException('Unsupported app artifact: ${entry.path}');
    }
  }
}

class _Replacement {
  _Replacement(this.target, this.directory);

  final String target;
  final bool directory;
  Directory? staging;
  bool saved = false;
  bool installed = false;

  String get staged => '${this.staging!.path}/${this.target.split('/').last}';
  String get backup => '${this.staging!.path}/previous';

  Future<void> stage(String source) async {
    final type = await FileSystemEntity.type(this.target, followLinks: false);
    if (type != FileSystemEntityType.notFound &&
        type !=
            (this.directory
                ? FileSystemEntityType.directory
                : FileSystemEntityType.file)) {
      throw FileSystemException(
        'Refusing non-regular destination ${this.target}. Legacy runner '
        'symlinks require a separately authorized migration; their targets '
        'and service configuration are not changed by install all.',
      );
    }
    final parent = Directory(File(this.target).parent.path);
    await parent.create(recursive: true);
    await for (final entry in parent.list(followLinks: false)) {
      if (entry.path.split('/').last.startsWith('.tuna-install-')) {
        throw FileSystemException(
          'Recovery artifacts found at ${entry.path}; keep apps stopped and '
          'inspect/restore the previous pair before retrying.',
        );
      }
    }
    this.staging = await parent.createTemp('.tuna-install-');
    if (this.directory) {
      await _copyDirectory(source, this.staged);
    } else {
      await File(source).copy(this.staged);
    }
  }

  Future<void> _rename(String source, String target) async {
    if (this.directory) {
      await Directory(source).rename(target);
    } else {
      await File(source).rename(target);
    }
  }

  Future<void> swap() async {
    if (await FileSystemEntity.type(this.target, followLinks: false) !=
        FileSystemEntityType.notFound) {
      await this._rename(this.target, this.backup);
      this.saved = true;
    }
    await this._rename(this.staged, this.target);
    this.installed = true;
  }

  Future<void> restore() async {
    if (this.installed) {
      await this._rename(this.target, this.staged);
      this.installed = false;
    }
    if (this.saved) {
      await this._rename(this.backup, this.target);
      this.saved = false;
    }
  }

  Future<void> clean() async {
    final staging = this.staging;
    if (staging != null) await staging.delete(recursive: true);
  }
}
