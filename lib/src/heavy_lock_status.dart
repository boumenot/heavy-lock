import 'dart:async';
import 'dart:convert';
import 'dart:ffi';
import 'dart:io';
import 'dart:typed_data';

typedef _MallocNative = Pointer<Void> Function(IntPtr);
typedef _MallocDart = Pointer<Void> Function(int);
typedef _FreeNative = Void Function(Pointer<Void>);
typedef _FreeDart = void Function(Pointer<Void>);
typedef _KillNative = Int32 Function(Int32, Int32);
typedef _KillDart = int Function(int, int);
typedef _ErrnoNative = Pointer<Int32> Function();
typedef _ErrnoDart = Pointer<Int32> Function();
typedef _OpenProcessNative = Pointer<Void> Function(Uint32, Int32, Uint32);
typedef _OpenProcessDart = Pointer<Void> Function(int, int, int);
typedef _CloseHandleNative = Int32 Function(Pointer<Void>);
typedef _CloseHandleDart = int Function(Pointer<Void>);

String? parseLinuxProcessStart(String stat) {
  final end = stat.lastIndexOf(')');
  if (end < 0) return null;
  final fields = stat.substring(end + 1).trim().split(RegExp(r'\s+'));
  if (fields.length <= 19 || fields[0] == 'Z' || fields[0] == 'X') {
    return null;
  }
  return int.tryParse(fields[19]) == null ? null : fields[19];
}

/// Returns the kernel's process start identity, or null if it cannot be read.
String? darwinProcessStart(int pid) {
  final libc = DynamicLibrary.process();
  final malloc = libc.lookupFunction<_MallocNative, _MallocDart>('malloc');
  final free = libc.lookupFunction<_FreeNative, _FreeDart>('free');
  final procInfo = DynamicLibrary.open('/usr/lib/libproc.dylib')
      .lookupFunction<
        Int32 Function(Int32, Int32, Uint64, Pointer<Void>, Int32),
        int Function(int, int, int, Pointer<Void>, int)
      >('proc_pidinfo');
  final buffer = malloc(136);
  if (buffer == nullptr) return null;
  try {
    if (procInfo(pid, 3, 0, buffer, 136) != 136) return null;
    final data = ByteData.sublistView(buffer.cast<Uint8>().asTypedList(136));
    if (data.getUint32(4, Endian.host) == 5) return null;
    return '${data.getUint64(120, Endian.host)}:'
        '${data.getUint64(128, Endian.host)}';
  } finally {
    free(buffer);
  }
}

bool _darwinProcessGone(int pid) {
  final libc = DynamicLibrary.process();
  final kill = libc.lookupFunction<_KillNative, _KillDart>('kill');
  final errno = libc.lookupFunction<_ErrnoNative, _ErrnoDart>('__error');
  return kill(pid, 0) == -1 && errno().value == 3;
}

String? _windowsProcessStart(int pid) {
  final kernel = DynamicLibrary.open('kernel32.dll');
  final open = kernel.lookupFunction<_OpenProcessNative, _OpenProcessDart>(
    'OpenProcess',
  );
  final close = kernel.lookupFunction<_CloseHandleNative, _CloseHandleDart>(
    'CloseHandle',
  );
  final getTimes = kernel
      .lookupFunction<
        Int32 Function(
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
        ),
        int Function(
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
          Pointer<Void>,
        )
      >('GetProcessTimes');
  final getExitCode = kernel
      .lookupFunction<
        Int32 Function(Pointer<Void>, Pointer<Uint32>),
        int Function(Pointer<Void>, Pointer<Uint32>)
      >('GetExitCodeProcess');
  final libc = DynamicLibrary.open('msvcrt.dll');
  final malloc = libc.lookupFunction<_MallocNative, _MallocDart>('malloc');
  final free = libc.lookupFunction<_FreeNative, _FreeDart>('free');
  final process = open(0x1000, 0, pid);
  if (process == nullptr) return null;
  final buffer = malloc(32);
  if (buffer == nullptr) {
    close(process);
    return null;
  }
  try {
    final bytes = buffer.cast<Uint8>();
    if (getExitCode(process, bytes.cast<Uint32>()) == 0 ||
        bytes.cast<Uint32>().value != 259) {
      return null;
    }
    if (getTimes(
          process,
          buffer,
          (bytes + 8).cast(),
          (bytes + 16).cast(),
          (bytes + 24).cast(),
        ) ==
        0) {
      return null;
    }
    final data = ByteData.sublistView(bytes.asTypedList(32));
    return 'windows:${data.getUint64(0, Endian.little)}';
  } finally {
    free(buffer);
    close(process);
  }
}

Future<String?> _processStart(int processId) async {
  if (Platform.isMacOS) return darwinProcessStart(processId);
  if (Platform.isWindows) return _windowsProcessStart(processId);
  if (Platform.isLinux) {
    try {
      final stat = await File('/proc/$processId/stat').readAsString();
      final start = parseLinuxProcessStart(stat);
      if (start == null) return null;
      final boot = (await File(
        '/proc/sys/kernel/random/boot_id',
      ).readAsString()).trim();
      return 'linux:$boot:$start';
    } on FileSystemException {
      return null;
    }
  }
  return null;
}

bool _processGone(int processId) {
  if (Platform.isWindows) {
    final kernel = DynamicLibrary.open('kernel32.dll');
    final open = kernel.lookupFunction<_OpenProcessNative, _OpenProcessDart>(
      'OpenProcess',
    );
    final lastError = kernel.lookupFunction<Uint32 Function(), int Function()>(
      'GetLastError',
    );
    final close = kernel.lookupFunction<_CloseHandleNative, _CloseHandleDart>(
      'CloseHandle',
    );
    final handle = open(0x1000, 0, processId);
    if (handle == nullptr) return lastError() == 87;
    close(handle);
    return false;
  }
  if (Platform.isMacOS) return _darwinProcessGone(processId);
  if (Platform.isLinux) {
    final libc = DynamicLibrary.process();
    final kill = libc.lookupFunction<_KillNative, _KillDart>('kill');
    final errno = libc.lookupFunction<_ErrnoNative, _ErrnoDart>(
      '__errno_location',
    );
    return kill(processId, 0) == -1 && errno().value == 3;
  }
  return false;
}

/// Publishes status without awaiting filesystem operations on the admission path.
final class HeavyLockStatus {
  HeavyLockStatus({
    required String gatePath,
    required this.worklog,
    required this.command,
    required this.weight,
    required this.capacity,
    required this.onError,
  }) : directory = Directory(
         '${gatePath.endsWith('.lock') ? gatePath.substring(0, gatePath.length - 5) : gatePath}.status',
       ),
       created = DateTime.now().toUtc();

  final Directory directory;
  final int? worklog;
  final List<String> command;
  final int weight;
  final int capacity;
  final void Function(Object error) onError;
  final DateTime created;
  String state = 'waiting';
  String? reason;
  DateTime? stateChanged;
  String? _identity;
  File? _file;
  Timer? _timer;
  Future<void> _pending = Future<void>.value();
  bool _closed = false;
  bool _ready = false;
  bool _reportedError = false;

  void start() {
    this._pending = this._initialize();
    this._timer = Timer.periodic(const Duration(seconds: 5), (_) {
      this._write();
    });
  }

  Future<void> _initialize() async {
    try {
      this._identity = await _processStart(pid);
      if (this._identity == null) {
        this._report(StateError('cannot identify runner process'));
        return;
      }
      if (this._closed) return;
      this._file = File(
        '${this.directory.path}${Platform.pathSeparator}$pid-${this._identity!.replaceAll(':', '-')}.json',
      );
      await this._prepareDirectory();
      this._ready = true;
      await this._prune();
      this._write();
    } on Object catch (error) {
      this._report(error);
    }
  }

  Future<void> _prepareDirectory() async {
    await this.directory.create(recursive: true);
    if (!Platform.isWindows &&
        (await this.directory.stat()).mode & 0x1ff != 0x1c0) {
      final chmod = await Process.run('chmod', ['700', this.directory.path]);
      if (chmod.exitCode != 0) {
        throw FileSystemException(
          'cannot restrict status directory',
          this.directory.path,
        );
      }
    }
  }

  Future<void> _prune() async {
    try {
      if (!await this.directory.exists()) return;
      await for (final entity in this.directory.list(followLinks: false)) {
        if (entity is! File || !entity.path.endsWith('.json')) continue;
        try {
          final record = jsonDecode(await entity.readAsString());
          if (record is! Map<String, dynamic>) continue;
          final otherPid = record['pid'];
          final recordedStart = record['processStart'];
          if (otherPid is! int || otherPid <= 0 || recordedStart is! String) {
            continue;
          }
          final actualStart = await _processStart(otherPid);
          if ((actualStart != null && actualStart != recordedStart) ||
              (actualStart == null && _processGone(otherPid))) {
            await entity.delete();
          }
        } on FormatException {
          continue;
        } on FileSystemException {
          continue;
        }
      }
    } on Object catch (error) {
      this._report(error);
    }
  }

  void update(String state, String? reason) {
    if (this.state != state ||
        this.reason != reason ||
        this.stateChanged == null) {
      this.state = state;
      this.reason = reason;
      this.stateChanged = DateTime.now().toUtc();
      this._write();
    }
  }

  void _write() {
    if (this._closed || !this._ready || this._file == null) return;
    final now = DateTime.now().toUtc();
    final payload = jsonEncode({
      'version': 1,
      'worklog': this.worklog,
      'attribution': this.worklog == null ? 'unknown' : 'declared',
      'command': this.command,
      'cwd': Directory.current.path,
      'weight': this.weight,
      'capacity': this.capacity,
      'state': this.state,
      'reason': this.reason,
      'pid': pid,
      'processStart': this._identity,
      'created': this.created.toIso8601String(),
      'stateChanged': (this.stateChanged ?? this.created).toIso8601String(),
      'heartbeat': now.toIso8601String(),
    });
    this._pending = this._pending.then((_) async {
      try {
        await this._prepareDirectory();
        if (this._closed) return;
        final temporary = File('${this._file!.path}.tmp');
        await temporary.writeAsString(payload);
        if (this._closed) {
          await temporary.delete();
          return;
        }
        await temporary.rename(this._file!.path);
      } on Object catch (error) {
        this._report(error);
      }
    });
  }

  void _report(Object error) {
    if (!this._reportedError) {
      this._reportedError = true;
      this.onError(error);
    }
  }

  Future<void> close() async {
    this._closed = true;
    this._timer?.cancel();
    try {
      await this._pending.timeout(const Duration(seconds: 1));
    } on Object catch (error) {
      this._report(error);
    }
    try {
      if (this._file != null && await this._file!.exists()) {
        await this._file!.delete().timeout(const Duration(seconds: 1));
      }
    } on Object catch (error) {
      this._report(error);
    }
  }
}
