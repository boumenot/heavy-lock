// Entry point: dart tool/dev.dart <command> [arguments]. See README.md.
import 'dart:io';

import 'lib/dev.dart';

Future<void> main(List<String> arguments) async {
  final root = File.fromUri(Platform.script).parent.parent.absolute.path;
  exitCode = await runDev(arguments, DevContext.host(root));
}
