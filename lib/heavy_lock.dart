// Admits heavy development work (full test suites, coverage, integration
// tests, app builds) from parallel sessions on one machine by weight and free
// resources, and runs it at lowered priority. Uses only `dart:` libraries so
// it runs without `pub get`.
//
//   heavy-lock [--weight heavy|light|<n>] [--no-wait] \
//       [--worklog <n>] \
//       -- <command> [arguments...]
//
// See README.md and docs/technical-decisions.md.
import 'dart:async';
import 'dart:ffi';
import 'dart:io';
import 'dart:math' as math;

import 'src/heavy_lock_status.dart';

const usageExitCode = 64;
const busyExitCode = 75;
const softwareExitCode = 70;
const sigintExitCode = 130;
const commandNotFoundExitCode = 127;

/// Overrides the gate lock file; slot and admission files live beside it.
const lockPathVariable = 'HEAVY_LOCK_PATH';

/// Set for the command so nested invocations on the same gate run directly
/// instead of waiting for themselves.
const heldLockVariable = 'HEAVY_LOCK_HELD';
const capacityVariable = 'HEAVY_LOCK_CAPACITY';
const minFreeMemoryVariable = 'HEAVY_LOCK_MIN_FREE_MEMORY_PERCENT';
const maxCpuLoadVariable = 'HEAVY_LOCK_MAX_CPU_LOAD_PERCENT';
const settleVariable = 'HEAVY_LOCK_SETTLE_SECONDS';
const priorityVariable = 'HEAVY_LOCK_PRIORITY';
const worklogVariable = 'HEAVY_LOCK_WORKLOG';

const heavyWeight = 2;
const lightWeight = 1;

const _usage =
    'Usage: heavy-lock [--weight heavy|light|<n>] [--no-wait] '
    '[--worklog <n>] '
    '-- <command> [arguments...]';

/// The gate shared by every heavy run on this machine. An exclusive holder
/// blocks this runner, which holds it shared while admitted.
String defaultLockPath(Map<String, String> environment) {
  if (Platform.isWindows) {
    final base = environment['LOCALAPPDATA'] ?? Directory.systemTemp.path;
    return '$base\\heavy-lock\\heavy-lock.lock';
  }
  final home = environment['HOME'] ?? Directory.systemTemp.path;
  return '$home/.cache/heavy-lock.lock';
}

/// Capacity in weight units. One heavy job fills the minimum capacity, so a
/// modest machine keeps one heavy job at a time.
int defaultCapacity({required int processors, required int? totalMemoryBytes}) {
  if (totalMemoryBytes == null) {
    return heavyWeight;
  }
  final memoryGiB = totalMemoryBytes ~/ (1 << 30);
  return math.min(processors ~/ 3, memoryGiB ~/ 4).clamp(heavyWeight, 8);
}

int? parseWeight(String value) => switch (value) {
  'heavy' => heavyWeight,
  'light' => lightWeight,
  _ => switch (int.tryParse(value)) {
    final weight? when weight >= 1 => weight,
    _ => null,
  },
};

/// A point-in-time view of machine load.
final class ResourceSample {
  const ResourceSample({
    required this.totalMemoryBytes,
    required this.availableMemoryBytes,
    required this.cpuLoadPercent,
  });

  final int totalMemoryBytes;
  final int availableMemoryBytes;

  /// Runnable work relative to the processor count (load average on POSIX,
  /// utilization on Windows). May exceed 100 on POSIX.
  final double cpuLoadPercent;

  double get freeMemoryPercent =>
      100 * this.availableMemoryBytes / this.totalMemoryBytes;

  @override
  String toString() =>
      '${this.freeMemoryPercent.toStringAsFixed(0)}% memory free, '
      'CPU load ${this.cpuLoadPercent.toStringAsFixed(0)}%';
}

/// Parses `sysctl -n hw.memsize vm.loadavg kern.memorystatus_level` (macOS).
///
/// `kern.memorystatus_level` is the kernel's free-memory percentage (the
/// figure `memory_pressure` reports), which accounts for compressed memory
/// that a `vm_stat` page count would understate.
ResourceSample? parseDarwinSample(String sysctl, {required int processors}) {
  final lines = sysctl.trim().split('\n');
  if (lines.length < 3) {
    return null;
  }
  final total = int.tryParse(lines[0].trim());
  final load = double.tryParse(
    lines[1].replaceAll(RegExp(r'[{}]'), '').trim().split(RegExp(r'\s+'))[0],
  );
  final freePercent = int.tryParse(lines[2].trim());
  if (total == null ||
      total <= 0 ||
      load == null ||
      freePercent == null ||
      freePercent < 0 ||
      freePercent > 100) {
    return null;
  }
  return ResourceSample(
    totalMemoryBytes: total,
    availableMemoryBytes: total * freePercent ~/ 100,
    cpuLoadPercent: 100 * load / processors,
  );
}

/// Parses `/proc/meminfo` and `/proc/loadavg` (Linux).
ResourceSample? parseLinuxSample(
  String meminfo,
  String loadavg, {
  required int processors,
}) {
  int? kib(String name) => int.tryParse(
    RegExp(
          '^$name:\\s+(\\d+) kB',
          multiLine: true,
        ).firstMatch(meminfo)?.group(1) ??
        '',
  );
  final total = kib('MemTotal');
  final available = kib('MemAvailable');
  final load = double.tryParse(loadavg.trim().split(RegExp(r'\s+'))[0]);
  if (total == null || total <= 0 || available == null || load == null) {
    return null;
  }
  return ResourceSample(
    totalMemoryBytes: total * 1024,
    availableMemoryBytes: available * 1024,
    cpuLoadPercent: 100 * load / processors,
  );
}

/// Parses `<total KiB> <free KiB> <CPU %>` printed by [_windowsSampleScript].
ResourceSample? parseWindowsSample(String output) {
  final fields = output.trim().split(RegExp(r'\s+'));
  if (fields.length != 3) {
    return null;
  }
  final total = int.tryParse(fields[0]);
  final free = int.tryParse(fields[1]);
  final cpu = double.tryParse(fields[2]);
  if (total == null || total <= 0 || free == null || cpu == null) {
    return null;
  }
  return ResourceSample(
    totalMemoryBytes: total * 1024,
    availableMemoryBytes: free * 1024,
    cpuLoadPercent: cpu,
  );
}

const _windowsSampleScript =
    r'$o = Get-CimInstance Win32_OperatingSystem; '
    r'$c = (Get-CimInstance Win32_Processor | '
    r'Measure-Object -Property LoadPercentage -Average).Average; '
    r'"$($o.TotalVisibleMemorySize) $($o.FreePhysicalMemory) $c"';

/// Samples the machine, or returns null when the platform tools fail.
Future<ResourceSample?> sampleResources() async {
  final processors = Platform.numberOfProcessors;
  try {
    if (Platform.isMacOS) {
      final sysctl = await Process.run('sysctl', [
        '-n',
        'hw.memsize',
        'vm.loadavg',
        'kern.memorystatus_level',
      ]);
      if (sysctl.exitCode != 0) {
        return null;
      }
      return parseDarwinSample(sysctl.stdout as String, processors: processors);
    }
    if (Platform.isWindows) {
      final result = await Process.run('powershell.exe', const [
        '-NoProfile',
        '-NonInteractive',
        '-Command',
        _windowsSampleScript,
      ]);
      return result.exitCode == 0
          ? parseWindowsSample(result.stdout as String)
          : null;
    }
    return parseLinuxSample(
      await File('/proc/meminfo').readAsString(),
      await File('/proc/loadavg').readAsString(),
      processors: processors,
    );
  } on Object {
    return null;
  }
}

final class HeavyLockConfig {
  const HeavyLockConfig({
    required this.gatePath,
    required this.capacity,
    required this.minFreeMemoryPercent,
    required this.maxCpuLoadPercent,
    required this.settle,
  });

  final String gatePath;
  final int capacity;
  final double minFreeMemoryPercent;
  final double maxCpuLoadPercent;

  /// Minimum time since the previous admission before sharing the machine,
  /// so the previous job's load shows up in the next sample.
  final Duration settle;

  String get _stem {
    final path = this.gatePath;
    return path.endsWith('.lock')
        ? path.substring(0, path.length - '.lock'.length)
        : path;
  }

  String slotPath(int index) => '${this._stem}.slot-$index.lock';

  /// Indices of every slot file that exists next to the gate, including those
  /// created by runs that computed a larger capacity than this one.
  Future<Set<int>> existingSlots() async {
    final directory = File(this.gatePath).parent;
    final prefix = '${File(this._stem).uri.pathSegments.last}.slot-';
    final indices = <int>{};
    if (!await directory.exists()) {
      return indices;
    }
    await for (final entry in directory.list(followLinks: false)) {
      final name = entry.uri.pathSegments.last;
      if (name.startsWith(prefix) && name.endsWith('.lock')) {
        final index = int.tryParse(
          name.substring(prefix.length, name.length - '.lock'.length),
        );
        if (index != null && index >= 0) {
          indices.add(index);
        }
      }
    }
    return indices;
  }

  String get admissionPath => '${this._stem}.admission.lock';

  String get admissionStatePath => '${this._stem}.admission.state';
}

/// Returns why a job that is not alone must wait, or null to admit it.
String? sharingRefusal({
  required HeavyLockConfig config,
  required ResourceSample? sample,
  required Duration sinceLastAdmission,
}) {
  if (sinceLastAdmission < config.settle) {
    return 'letting the previous admission settle';
  }
  if (sample == null) {
    return 'machine load unavailable; waiting to run alone';
  }
  if (sample.freeMemoryPercent < config.minFreeMemoryPercent ||
      sample.cpuLoadPercent > config.maxCpuLoadPercent) {
    return 'waiting for free resources ($sample)';
  }
  return null;
}

Future<bool> _tryLock(RandomAccessFile file, FileLock mode) async {
  try {
    await file.lock(mode);
    return true;
  } on FileSystemException {
    return false;
  }
}

Future<RandomAccessFile> _openLockFile(String path) async {
  final file = File(path);
  await file.parent.create(recursive: true);
  return file.open(mode: FileMode.append);
}

/// Lock files held for an admitted job. The OS releases them if the process
/// ends first; the files themselves are never deleted.
final class HeavySlots {
  HeavySlots._(this.weight, this._files);

  final int weight;
  final List<RandomAccessFile> _files;

  Future<void> release() async {
    for (final file in this._files.reversed) {
      await file.close();
    }
  }
}

/// Weighted admission over capacity slot files.
///
/// Only the holder of the admission lock acquires slots, so jobs never
/// deadlock: running jobs only release slots, and the queue head waits for
/// them. A legacy exclusive holder of the gate excludes every admitted job,
/// and an admitted job (holding the gate shared) excludes legacy holders.
final class WeightedAdmission {
  WeightedAdmission(this.config, {this.sampler = sampleResources});

  final HeavyLockConfig config;
  final Future<ResourceSample?> Function() sampler;
  static const _slotPoll = Duration(seconds: 1);
  static const _resourcePoll = Duration(seconds: 5);

  /// Returns null only when [wait] is false and the job is not admitted now.
  Future<HeavySlots?> admit({
    required int weight,
    required bool wait,
    required void Function(String reason) onWaiting,
  }) async {
    final config = this.config;
    final admission = await _openLockFile(config.admissionPath);
    final held = <RandomAccessFile>[];
    RandomAccessFile? gate;
    var admitted = false;
    try {
      if (!await _tryLock(admission, FileLock.exclusive)) {
        if (!wait) {
          return null;
        }
        onWaiting('queued behind another heavy run being admitted');
        await admission.lock(FileLock.blockingExclusive);
      }
      gate = await _openLockFile(config.gatePath);
      if (!await _tryLock(gate, FileLock.shared)) {
        if (!wait) {
          return null;
        }
        onWaiting('${config.gatePath} is held exclusively by another run');
        await gate.lock(FileLock.blockingShared);
      }
      final slots = <int, RandomAccessFile>{};
      while (true) {
        for (
          var index = 0;
          index < config.capacity && slots.length < weight;
          index++
        ) {
          if (slots.containsKey(index)) {
            continue;
          }
          final slot = await _openLockFile(config.slotPath(index));
          if (await _tryLock(slot, FileLock.exclusive)) {
            slots[index] = slot;
            held.add(slot);
          } else {
            await slot.close();
          }
        }
        if (slots.length < weight) {
          if (!wait) {
            return null;
          }
          onWaiting(
            'waiting for $weight of ${config.capacity} slots '
            '(${config.capacity - slots.length} in use by other runs)',
          );
          await Future<void>.delayed(_slotPoll);
          continue;
        }
        if (await this._othersRunning(slots.keys.toSet())) {
          final refusal = sharingRefusal(
            config: config,
            sample: await this.sampler(),
            sinceLastAdmission: await this._sinceLastAdmission(),
          );
          if (refusal != null) {
            if (!wait) {
              return null;
            }
            onWaiting(refusal);
            await Future<void>.delayed(_resourcePoll);
            continue;
          }
        }
        break;
      }
      await File(config.admissionStatePath)
          .writeAsString('${DateTime.now().millisecondsSinceEpoch}\n');
      admitted = true;
      return HeavySlots._(weight, [gate, ...held]);
    } finally {
      await admission.close();
      if (!admitted) {
        for (final file in held) {
          await file.close();
        }
        await gate?.close();
      }
    }
  }

  // Scans every existing slot file, not only this run's capacity, so a run
  // that computed a smaller capacity (for example after a failed resource
  // sample) still sees jobs holding higher slots and applies the resource
  // check before sharing.
  Future<bool> _othersRunning(Set<int> own) async {
    final indices = {
      for (var index = 0; index < this.config.capacity; index++) index,
      ...await this.config.existingSlots(),
    };
    for (final index in indices) {
      if (own.contains(index)) {
        continue;
      }
      final slot = await _openLockFile(this.config.slotPath(index));
      try {
        if (!await _tryLock(slot, FileLock.exclusive)) {
          return true;
        }
      } finally {
        await slot.close();
      }
    }
    return false;
  }

  Future<Duration> _sinceLastAdmission() async {
    final file = File(this.config.admissionStatePath);
    final last = await file.exists()
        ? int.tryParse((await file.readAsString()).trim())
        : null;
    if (last == null) {
      return const Duration(days: 1);
    }
    final since = Duration(
      milliseconds: DateTime.now().millisecondsSinceEpoch - last,
    );
    return since.isNegative ? const Duration(days: 1) : since;
  }
}

void _log(String message) => stderr.writeln('heavy-lock: $message');

/// Windows process control through kernel32, without package:ffi.
final class _Windows {
  _Windows._(this._kernel32);

  static _Windows? open() {
    try {
      return _Windows._(DynamicLibrary.open('kernel32.dll'));
    } on Object {
      return null;
    }
  }

  final DynamicLibrary _kernel32;

  late final _currentProcess = this._kernel32
      .lookupFunction<IntPtr Function(), int Function()>('GetCurrentProcess');

  late final _setPriorityClass = this._kernel32
      .lookupFunction<Int32 Function(IntPtr, Uint32), int Function(int, int)>(
        'SetPriorityClass',
      );

  late final _createJobObject = this._kernel32
      .lookupFunction<
        IntPtr Function(Pointer<Void>, Pointer<Void>),
        int Function(Pointer<Void>, Pointer<Void>)
      >('CreateJobObjectW');

  late final _setInformationJobObject = this._kernel32
      .lookupFunction<
        Int32 Function(IntPtr, Int32, Pointer<Uint8>, Uint32),
        int Function(int, int, Pointer<Uint8>, int)
      >('SetInformationJobObject');

  late final _assignProcessToJobObject = this._kernel32
      .lookupFunction<Int32 Function(IntPtr, IntPtr), int Function(int, int)>(
        'AssignProcessToJobObject',
      );

  late final _localAlloc = this._kernel32
      .lookupFunction<
        Pointer<Uint8> Function(Uint32, IntPtr),
        Pointer<Uint8> Function(int, int)
      >('LocalAlloc');

  /// New child processes inherit a below-normal class from their creator.
  bool lowerPriority() {
    const belowNormalPriorityClass = 0x00004000;
    return this._setPriorityClass(
          this._currentProcess(),
          belowNormalPriorityClass,
        ) !=
        0;
  }

  int? _job;
  Pointer<Uint8>? _limits;

  // JOBOBJECT_EXTENDED_LIMIT_INFORMATION is 144 bytes on 64-bit Windows;
  // BasicLimitInformation.LimitFlags is the DWORD at byte offset 16.
  static const _extendedLimitInformation = 9;
  static const _limitsSize = 144;
  static const _killOnJobClose = 0x00002000;

  bool _setKillOnClose(bool enabled) {
    this._limits!.cast<Uint32>()[4] = enabled ? _killOnJobClose : 0;
    return this._setInformationJobObject(
          this._job!,
          _extendedLimitInformation,
          this._limits!,
          _limitsSize,
        ) !=
        0;
  }

  /// Puts this process, and so every process it starts, in a job that the
  /// OS terminates if this process dies without [releaseChildren].
  bool killChildrenOnExit() {
    const zeroInitialized = 0x0040;
    final job = this._createJobObject(nullptr, nullptr);
    final limits = this._localAlloc(zeroInitialized, _limitsSize);
    if (job == 0 || limits == nullptr) {
      return false;
    }
    this._job = job;
    this._limits = limits;
    return this._setKillOnClose(true) &&
        this._assignProcessToJobObject(job, this._currentProcess()) != 0;
  }

  /// Lets processes the command left behind (for example build daemons)
  /// outlive a normal exit.
  void releaseChildren() {
    if (this._job != null) {
      this._setKillOnClose(false);
    }
  }
}

/// One `ps` row: the parent pid, the state (`T...` when stopped) and the start
/// time that, with the pid, identifies the process.
typedef ProcessEntry = ({int parent, String state, String started});

/// Every live (non-zombie) process, from `ps` (POSIX only).
Future<Map<int, ProcessEntry>?> processTable() async {
  final ProcessResult result;
  try {
    result = await Process.run('ps', const [
      '-A',
      '-o',
      'pid=',
      '-o',
      'ppid=',
      '-o',
      'stat=',
      '-o',
      'lstart=',
    ]);
  } on ProcessException {
    return null;
  }
  if (result.exitCode != 0) {
    return null;
  }
  return parseProcessTable(result.stdout as String);
}

/// Parses `ps -o pid= -o ppid= -o stat= -o lstart=` output, without zombies.
Map<int, ProcessEntry> parseProcessTable(String output) {
  final line = RegExp(r'^\s*(\d+)\s+(\d+)\s+(\S+)\s+(\S.*?)\s*$');
  return {
    for (final match in output.split('\n').map(line.firstMatch).nonNulls)
      if (!match[3]!.startsWith('Z'))
        int.parse(match[1]!): (
          parent: int.parse(match[2]!),
          state: match[3]!,
          started: match[4]!,
        ),
  };
}

/// The processes a command started, tracked while it runs so descendants
/// that are reparented (for example after their parent exits) stay known.
/// A process is identified by its pid and start time, so a reused pid is
/// never mistaken for a tracked one.
///
/// Signals are delivered with the tree frozen: a `ps` snapshot is
/// stale as soon as it is taken, so a tracked process could fork after it and
/// then exit on the signal, orphaning a child the runner never saw. Stopped
/// processes can neither fork nor exit, so the tree is stopped until two
/// consecutive snapshots agree before the signal is sent.
final class ProcessTree {
  ProcessTree(
    this._root, {
    this._snapshot = processTable,
    this._kill = Process.killPid,
    this.freezeLimit = const Duration(seconds: 2),
  });

  final int _root;
  final Future<Map<int, ProcessEntry>?> Function() _snapshot;
  final bool Function(int, ProcessSignal) _kill;

  /// How long [signal] waits for the tree to stop before signalling anyway.
  final Duration freezeLimit;
  final _tracked = <int, String>{};
  final _signalled = <int>{};
  var _seeded = false;

  /// Updates the tracked set from a fresh `ps` snapshot and returns it.
  Future<Set<int>> refresh() async {
    final table = await this._snapshot();
    if (table != null) {
      this._update(table);
    }
    return this._tracked.keys.toSet();
  }

  /// Whether a tracked process has not been signalled yet.
  bool get hasUnsignalled =>
      this._tracked.keys.any((pid) => !this._signalled.contains(pid));

  void _update(Map<int, ProcessEntry> table) {
    if (!this._seeded) {
      this._seeded = true;
      final root = table[this._root];
      if (root != null) {
        this._tracked[this._root] = root.started;
      }
    }
    this._tracked.removeWhere((pid, started) => table[pid]?.started != started);
    final children = <int, List<int>>{};
    table.forEach((pid, entry) => (children[entry.parent] ??= []).add(pid));
    final pending = this._tracked.keys.toList();
    while (pending.isNotEmpty) {
      for (final child in children[pending.removeLast()] ?? const <int>[]) {
        if (!this._tracked.containsKey(child)) {
          this._tracked[child] = table[child]!.started;
          pending.add(child);
        }
      }
    }
  }

  /// Stops every tracked process, adding those it stopped to [stopped], until
  /// two consecutive snapshots show the same tracked set, all stopped. `ps`
  /// is not atomic on every platform, so one snapshot showing a stopped
  /// parent may still miss the child it forked just before stopping.
  /// Returns false if that did not happen within [freezeLimit].
  Future<bool> _freeze(Set<int> stopped) async {
    final deadline = DateTime.now().add(this.freezeLimit);
    Set<int>? previous;
    while (true) {
      final table = await this._snapshot();
      if (table == null) {
        return false;
      }
      this._update(table);
      final tracked = this._tracked.keys.toSet();
      var running = false;
      for (final pid in tracked) {
        if (!table[pid]!.state.startsWith('T')) {
          running = true;
          if (this._kill(pid, ProcessSignal.sigstop)) {
            stopped.add(pid);
          }
        }
      }
      if (!running &&
          previous != null &&
          previous.length == tracked.length &&
          previous.containsAll(tracked)) {
        return true;
      }
      previous = running ? null : tracked;
      if (!DateTime.now().isBefore(deadline)) {
        return false;
      }
      if (running) {
        await Future<void>.delayed(const Duration(milliseconds: 10));
      }
    }
  }

  /// Sends [signal] to every tracked process (with [onlyNew], only to those
  /// not signalled before) while the tree is frozen, then lets the processes
  /// it stopped continue so they can act on it. Returns false when the tree
  /// could not be frozen and the signal was delivered from the latest
  /// snapshot instead.
  Future<bool> signal(ProcessSignal signal, {bool onlyNew = false}) async {
    final stopped = <int>{};
    try {
      final frozen = await this._freeze(stopped);
      for (final pid in this._tracked.keys) {
        if (this._signalled.add(pid) || !onlyNew) {
          this._kill(pid, signal);
        }
      }
      return frozen;
    } finally {
      // Descendants first: a parent that exits while a descendant's process
      // group is still stopped orphans that group, and the kernel then sends
      // it SIGHUP. A parent is always tracked before its descendants.
      final order = [
        ...stopped.where((pid) => !this._tracked.containsKey(pid)),
        ...this._tracked.keys.toList().reversed.where(stopped.contains),
      ];
      for (final pid in order) {
        this._kill(pid, ProcessSignal.sigcont);
      }
    }
  }
}

/// Termination signals, watched for the runner's whole life so that none of
/// them falls back to the default handler (which would end the runner and
/// release its locks) between phases.
final class _Signals {
  _Signals() {
    for (final signal
        in Platform.isWindows
            ? [ProcessSignal.sigint]
            : [
                ProcessSignal.sigint,
                ProcessSignal.sigterm,
                ProcessSignal.sighup,
              ]) {
      signal.watch().listen((signal) => this.handler(signal));
    }
  }

  /// Exits as a shell reports a signal; replaced while a command runs.
  void Function(ProcessSignal) handler = (signal) =>
      exit(signal == ProcessSignal.sigint ? 130 : 143);
}

/// How a command ended: its exit code, and whether the run was cancelled by
/// a termination signal.
typedef _Outcome = ({int code, bool cancelled});

/// Grace period between forwarding a termination signal and killing what is
/// left of the command.
const _stopGrace = Duration(seconds: 20);

Future<_Outcome> _runCommand(
  List<String> command,
  Map<String, String> environment,
  _Signals signals, {
  List<String> prefix = const [],
  bool jobArmed = false,
}) async {
  final launch = [...prefix, ...command];
  ProcessSignal? early;
  signals.handler = (signal) => early ??= signal;
  final Process process;
  try {
    process = await Process.start(
      launch.first,
      launch.sublist(1),
      environment: environment,
      mode: ProcessStartMode.inheritStdio,
      // Lets Windows resolve .bat/.cmd tools such as flutter and fvm.
      runInShell: Platform.isWindows,
    );
  } on ProcessException catch (error) {
    _log('could not start ${command.first}: ${error.message}');
    return (code: commandNotFoundExitCode, cancelled: early != null);
  }
  int? code;
  final exited = process.exitCode.then((value) => code = value);
  // On POSIX, the tree is tracked for the whole run and termination signals
  // go to every process in it. Windows delivers Ctrl+C to the console group,
  // and the kill-on-close job covers what survives a cancelled run.
  final tree = Platform.isWindows ? null : ProcessTree(process.pid);
  await tree?.refresh();
  var cancelled = false;
  ProcessSignal? pendingSignal;
  DateTime? deadline;
  // Keep the handler installed until cleanup ends, so no signal ends the
  // runner while the tree lives.
  void onSignal(ProcessSignal signal) {
    if (!cancelled) {
      cancelled = true;
      deadline = DateTime.now().add(_stopGrace);
      pendingSignal = signal;
    } else {
      _log('stopping the command now.');
      deadline = DateTime.now();
    }
  }

  signals.handler = onSignal;
  if (early != null) {
    // Arrived while the command was starting.
    if (Platform.isWindows) {
      process.kill();
    }
    onSignal(early!);
  }
  DateTime? giveUp;
  var killed = false;
  var unfrozen = false;
  Future<void> deliver(ProcessSignal signal, {bool onlyNew = false}) async {
    if (!await tree!.signal(signal, onlyNew: onlyNew) && !unfrozen) {
      unfrozen = true;
      _log('could not stop the whole command tree; signalling it anyway.');
    }
  }

  while (true) {
    final alive = await tree?.refresh() ?? const <int>{};
    if (cancelled && tree != null && alive.isNotEmpty) {
      if (pendingSignal case final signal?) {
        pendingSignal = null;
        await deliver(signal);
      }
      if (!DateTime.now().isBefore(deadline!)) {
        if (giveUp == null) {
          _log('killing ${alive.length} remaining process(es).');
        }
        giveUp ??= DateTime.now().add(const Duration(seconds: 5));
        await deliver(ProcessSignal.sigkill);
      } else if (tree.hasUnsignalled) {
        await deliver(ProcessSignal.sigterm, onlyNew: true);
      }
    }
    if (cancelled &&
        tree == null &&
        code == null &&
        !DateTime.now().isBefore(deadline!)) {
      // Windows: the command ignored Ctrl+C. With the kill-on-close job
      // armed, return so the caller exits and Windows terminates the whole
      // tree as the locks are released. Otherwise terminate the command and
      // keep the locks until it has exited.
      if (jobArmed) {
        _log('stopping the command now.');
        code = sigintExitCode;
        break;
      }
      if (!killed) {
        killed = true;
        _log('terminating the command.');
        process.kill();
      }
    }
    if (code != null &&
        (!cancelled ||
            alive.isEmpty ||
            (giveUp != null && !DateTime.now().isBefore(giveUp)))) {
      break;
    }
    await Future.any([
      exited,
      Future<void>.delayed(
        cancelled
            ? const Duration(milliseconds: 250)
            : const Duration(seconds: 1),
      ),
    ]);
  }
  // Later signals no longer matter to the command; the caller releases the
  // locks and exits.
  signals.handler = (_) {};
  final result = code!;
  return (
    // A negative POSIX code is a termination signal: report it as a shell
    // does. Windows reports NTSTATUS failures as negative codes; keep them.
    code: result < 0 && !Platform.isWindows ? 128 - result : result,
    cancelled: cancelled,
  );
}

/// How the admitted command is deprioritized.
enum HeavyPriority {
  /// macOS background QoS (`taskpolicy -b`), which also throttles I/O and
  /// timers; elsewhere the same as [low]. Opt-in: on the reference machine it
  /// more than doubled test wall time and caused test failures.
  background,

  /// `nice -n 10` on POSIX, below-normal priority class on Windows.
  low,
  normal;

  static HeavyPriority? parse(String? value) => switch (value) {
    null || '' || 'low' => low,
    'background' => background,
    'normal' => normal,
    _ => null,
  };
}

const _taskpolicy = '/usr/sbin/taskpolicy';

bool _hasTaskpolicy() => Platform.isMacOS && File(_taskpolicy).existsSync();

/// The launcher prefix that applies [priority] on POSIX; Windows lowers the
/// runner's own priority class instead.
List<String> posixPriorityPrefix(
  HeavyPriority priority, {
  required bool hasTaskpolicy,
}) {
  if (Platform.isWindows || priority == HeavyPriority.normal) {
    return const [];
  }
  if (priority == HeavyPriority.background && hasTaskpolicy) {
    return const [_taskpolicy, '-b'];
  }
  return const ['nice', '-n', '10'];
}

double _percent(
  Map<String, String> environment,
  String name,
  double fallback,
) => double.tryParse(environment[name] ?? '') ?? fallback;

Future<int> run(List<String> arguments) async {
  if (arguments.length == 1 &&
      (arguments.single == '--help' || arguments.single == '-h')) {
    stdout.writeln(_usage);
    return 0;
  }
  var noWait = false;
  int? weight;
  int? worklog;
  final separator = arguments.indexOf('--');
  final options = separator < 0 ? arguments : arguments.sublist(0, separator);
  for (var index = 0; index < options.length; index++) {
    switch (options[index]) {
      case '--no-wait' when !noWait:
        noWait = true;
      case '--weight' when weight == null && index + 1 < options.length:
        weight = parseWeight(options[++index]);
        if (weight == null) {
          stderr.writeln(_usage);
          return usageExitCode;
        }
      case '--worklog' when worklog == null && index + 1 < options.length:
        worklog = int.tryParse(options[++index]);
        if (worklog == null || worklog < 1) {
          stderr.writeln(_usage);
          return usageExitCode;
        }
      default:
        stderr.writeln(_usage);
        return usageExitCode;
    }
  }
  if (separator < 0 || separator == arguments.length - 1) {
    stderr.writeln(_usage);
    return usageExitCode;
  }
  final command = arguments.sublist(separator + 1);
  final environment = Platform.environment;
  if (worklog == null && environment.containsKey(worklogVariable)) {
    worklog = int.tryParse(environment[worklogVariable]!);
    if (worklog == null || worklog < 1) {
      _log('$worklogVariable must be a positive integer.');
      return usageExitCode;
    }
  }
  final gatePath = File(
    environment[lockPathVariable] ?? defaultLockPath(environment),
  ).absolute.path;
  final childEnvironment = {heldLockVariable: gatePath};

  if (environment[heldLockVariable] == gatePath) {
    _log('already admitted by a parent run; running directly.');
    return (await _runCommand(command, childEnvironment, _Signals())).code;
  }

  final configuredCapacity = int.tryParse(environment[capacityVariable] ?? '');
  final capacity = configuredCapacity != null && configuredCapacity >= 1
      ? configuredCapacity
      : defaultCapacity(
          processors: Platform.numberOfProcessors,
          totalMemoryBytes: (await sampleResources())?.totalMemoryBytes,
        );
  final config = HeavyLockConfig(
    gatePath: gatePath,
    capacity: capacity,
    minFreeMemoryPercent: _percent(environment, minFreeMemoryVariable, 25),
    maxCpuLoadPercent: _percent(environment, maxCpuLoadVariable, 50),
    settle: Duration(
      seconds: int.tryParse(environment[settleVariable] ?? '') ?? 60,
    ),
  );
  final priority = HeavyPriority.parse(environment[priorityVariable]);
  if (priority == null) {
    _log('$priorityVariable must be background, low or normal.');
    return usageExitCode;
  }
  final requested = math.min(weight ?? heavyWeight, capacity);

  final started = DateTime.now();
  String? lastReason;
  // Nothing has started while waiting, so a signal simply exits; the OS
  // releases any lock taken so far.
  final signals = _Signals();
  final status = HeavyLockStatus(
    gatePath: gatePath,
    worklog: worklog,
    command: command,
    weight: requested,
    capacity: capacity,
    onError: (error) => _log('status unavailable: $error'),
  )..start();
  ProcessSignal? waitingSignal;
  signals.handler = (signal) {
    if (waitingSignal != null) return;
    waitingSignal = signal;
    unawaited(status.close());
    exit(signal == ProcessSignal.sigint ? 130 : 143);
  };
  HeavySlots? slots;
  try {
    slots = await WeightedAdmission(config).admit(
      weight: requested,
      wait: !noWait,
      onWaiting: (reason) {
        if (reason != lastReason) {
          lastReason = reason;
          _log('$reason...');
          status.update('waiting', reason);
        }
      },
    );
  } on Object {
    await status.close();
    rethrow;
  }
  if (waitingSignal != null) {
    await slots?.release();
    await status.close();
    return waitingSignal == ProcessSignal.sigint ? 130 : 143;
  }
  if (slots == null) {
    _log('not admitted now; another heavy run is active.');
    await status.close();
    return busyExitCode;
  }
  status.update('running', null);
  final waited = DateTime.now().difference(started).inSeconds;
  _log(
    'admitted with weight $requested of $capacity'
    '${lastReason == null ? '' : ' after ${waited}s'}.',
  );
  final windows = Platform.isWindows ? _Windows.open() : null;
  var jobArmed = false;
  try {
    if (windows != null) {
      if (priority != HeavyPriority.normal && !windows.lowerPriority()) {
        _log('could not lower priority; continuing.');
      }
      jobArmed = windows.killChildrenOnExit();
      if (!jobArmed) {
        _log('could not tie the command to this process; continuing.');
      }
    }
    final outcome = await _runCommand(
      command,
      childEnvironment,
      signals,
      prefix: posixPriorityPrefix(priority, hasTaskpolicy: _hasTaskpolicy()),
      jobArmed: jobArmed,
    );
    if (windows != null) {
      if (outcome.cancelled && jobArmed) {
        // Exit with the job still armed: Windows terminates whatever the
        // command left running as this process ends and its locks are
        // released, instead of releasing the slots first.
        unawaited(status.close());
        await stdout.flush();
        await stderr.flush();
        exit(outcome.code);
      }
      windows.releaseChildren();
    }
    return outcome.code;
  } finally {
    await slots.release();
    await status.close();
  }
}

Future<void> main(List<String> arguments) async {
  int code;
  try {
    code = await run(arguments);
  } on Object catch (error) {
    // The lifetime signal watchers keep the isolate alive, so always exit.
    _log('failed: $error');
    code = softwareExitCode;
  }
  await stdout.flush();
  await stderr.flush();
  exit(code);
}
