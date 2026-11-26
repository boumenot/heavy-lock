import 'dart:convert';
import 'dart:io';

import 'package:heavy_lock/heavy_lock.dart';

Future<void> main(List<String> arguments) async {
  if (arguments.length == 1 &&
      (arguments.single == '--help' || arguments.single == '-h')) {
    stdout.writeln('Usage: heavy-lock-status [--json]');
    return;
  }
  if (arguments.isNotEmpty &&
      (arguments.length != 1 || arguments.single != '--json')) {
    stderr.writeln('Usage: heavy-lock-status [--json]');
    exitCode = usageExitCode;
    return;
  }
  final gate = File(
    Platform.environment[lockPathVariable] ??
        defaultLockPath(Platform.environment),
  ).absolute.path;
  final stem = gate.endsWith('.lock')
      ? gate.substring(0, gate.length - 5)
      : gate;
  final directory = Directory('$stem.status');
  final records = <Map<String, dynamic>>[];
  try {
    if (await directory.exists()) {
      await for (final entity in directory.list(followLinks: false)) {
        if (entity is! File || !entity.path.endsWith('.json')) continue;
        final String content;
        try {
          content = await entity.readAsString();
        } on PathNotFoundException {
          // A runner can finish between listing and reading its record.
          continue;
        }
        final record = jsonDecode(content);
        if (record is! Map<String, dynamic> || record['version'] != 1) {
          throw FormatException('Unsupported status record: ${entity.path}');
        }
        records.add(record);
      }
    }
    records.sort((a, b) => '${a['created']}'.compareTo('${b['created']}'));
    if (arguments.isNotEmpty) {
      stdout.writeln(jsonEncode(records));
    } else {
      stdout.writeln('Gate: $gate');
      stdout.writeln(
        'Reported status only; records may be stale. Not a lock probe.',
      );
      if (records.isEmpty) stdout.writeln('No status records.');
      for (final record in records) {
        stdout.writeln(
          'PID ${record['pid']}  ${record['state']}  '
          'worklog #${record['worklog'] ?? '?'}  '
          'weight ${record['weight']}/${record['capacity']}',
        );
        stdout.writeln('  cwd: ${jsonEncode(record['cwd'])}');
        stdout.writeln('  command: ${jsonEncode(record['command'])}');
        if (record['reason'] != null) {
          stdout.writeln('  reason: ${jsonEncode(record['reason'])}');
        }
        stdout.writeln('  heartbeat: ${record['heartbeat']}');
      }
    }
  } on Object catch (error) {
    stderr.writeln('heavy-lock-status: failed: $error');
    exitCode = softwareExitCode;
  }
}
