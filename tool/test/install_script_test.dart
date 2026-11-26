import 'dart:io';

import 'package:test/test.dart';

void main() {
  late Directory temporary;
  late String root;
  late String home;
  late Map<String, String> environment;

  Future<ProcessResult> runScript(String name, [List<String> args = const []]) {
    return Process.run(
      'bash',
      ['$root/tool/$name', ...args],
      environment: environment,
      includeParentEnvironment: false,
    );
  }

  setUp(() async {
    temporary = await Directory.systemTemp.createTemp('heavy_lock_install_');
    root = '${temporary.path}/source checkout';
    home = '${temporary.path}/home';
    await Directory('$root/tool').create(recursive: true);
    await Directory('$root/build/heavy-lock').create(recursive: true);
    await Directory(home).create();
    for (final script in ['build.sh', 'install.sh']) {
      await File('$script').copy('$root/tool/$script');
    }
    environment = {...Platform.environment, 'HOME': home};
  });

  tearDown(() async {
    await temporary.delete(recursive: true);
  });

  test(
    'installs to the default directory and replaces it on upgrade',
    () async {
      final built = File('$root/build/heavy-lock/heavy-lock');
      await built.writeAsString('first binary');
      var result = await runScript('install.sh');
      expect(result.exitCode, 0, reason: '${result.stderr}');
      final installed = File('$home/.local/bin/heavy-lock');
      expect(await installed.readAsString(), 'first binary');
      expect((await installed.stat()).mode & 0x49, isNonZero);

      await built.writeAsString('upgraded binary');
      result = await runScript('install.sh');
      expect(result.exitCode, 0, reason: '${result.stderr}');
      expect(await installed.readAsString(), 'upgraded binary');
    },
    skip: Platform.isWindows,
  );

  test('failed installation preserves the existing executable', () async {
    final destination = Directory('${temporary.path}/custom destination');
    await destination.create();
    final installed = File('${destination.path}/heavy-lock');
    await installed.writeAsString('working binary');

    final result = await runScript('install.sh', [destination.path]);
    expect(result.exitCode, isNot(0));
    expect(result.stderr, isNotEmpty);
    expect(result.stdout, isNot(contains('Installed:')));
    expect(await installed.readAsString(), 'working binary');
    expect(await destination.list().map((entry) => entry.path).toList(), [
      installed.path,
    ]);
  }, skip: Platform.isWindows);

  test('rejects a directory at the installation target', () async {
    await File('$root/build/heavy-lock/heavy-lock').writeAsString('binary');
    final destination = Directory('${temporary.path}/destination');
    await Directory('${destination.path}/heavy-lock').create(recursive: true);

    final result = await runScript('install.sh', [destination.path]);
    expect(result.exitCode, isNot(0));
    expect(result.stderr, isNotEmpty);
    expect(result.stdout, isNot(contains('Installed:')));
    expect(
      await Directory('${destination.path}/heavy-lock').list().isEmpty,
      isTrue,
    );
  }, skip: Platform.isWindows);

  test('failed copy does not replace the installed executable', () async {
    final fakeBin = Directory('${temporary.path}/fake bin');
    await fakeBin.create();
    final fakeInstall = File('${fakeBin.path}/install');
    await fakeInstall.writeAsString('''#!/bin/sh
for arg do last="\$arg"; done
printf partial > "\$last"
exit 23
''');
    final chmod = await Process.run('chmod', ['+x', fakeInstall.path]);
    expect(chmod.exitCode, 0, reason: '${chmod.stderr}');
    environment['PATH'] = '${fakeBin.path}:${Platform.environment['PATH']}';
    await File('$root/build/heavy-lock/heavy-lock').writeAsString('new binary');
    final destination = Directory('${temporary.path}/destination');
    await destination.create();
    final installed = File('${destination.path}/heavy-lock');
    await installed.writeAsString('working binary');

    final result = await runScript('install.sh', [destination.path]);
    expect(result.exitCode, isNot(0));
    expect(result.stderr, isNotEmpty);
    expect(result.stdout, isNot(contains('Installed:')));
    expect(await installed.readAsString(), 'working binary');
    expect(await destination.list().map((entry) => entry.path).toList(), [
      installed.path,
    ]);
  }, skip: Platform.isWindows);

  test('build.sh delegates to the orchestrator in the Nix shell', () async {
    final fakeBin = Directory('${temporary.path}/fake bin');
    await fakeBin.create();
    final nix = File('${fakeBin.path}/nix');
    await nix.writeAsString('''#!/bin/sh
printf '%s\\n' "\$@" > "\$NIX_ARGUMENTS"
exit 9
''');
    final chmod = await Process.run('chmod', ['+x', nix.path]);
    expect(chmod.exitCode, 0, reason: '${chmod.stderr}');
    environment['PATH'] = '${fakeBin.path}:${Platform.environment['PATH']}';
    environment['NIX_ARGUMENTS'] = '${temporary.path}/nix-arguments';

    final result = await runScript('build.sh');
    expect(result.exitCode, 9);
    expect(await File(environment['NIX_ARGUMENTS']!).readAsLines(), [
      'develop',
      root,
      '-c',
      'dart',
      '$root/tool/dev.dart',
      'build',
      'heavy-lock',
    ]);
  }, skip: Platform.isWindows);
}
