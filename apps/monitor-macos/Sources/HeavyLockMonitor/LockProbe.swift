import Darwin
import Foundation

enum LockProbe {
    enum Result: Equatable {
        case free, held(Int32?), unavailable
    }

    static func conflict(path: String, requestWrite: Bool) -> Result {
        let fd = open(path, O_RDONLY)
        guard fd >= 0 else { return errno == ENOENT ? .free : .unavailable }
        defer { close(fd) }
        var lock = flock(
            l_start: 0, l_len: 0, l_pid: 0,
            l_type: Int16(requestWrite ? F_WRLCK : F_RDLCK),
            l_whence: Int16(SEEK_SET)
        )
        guard fcntl(fd, F_GETLK, &lock) == 0 else { return .unavailable }
        return lock.l_type == Int16(F_UNLCK) ? .free : .held(lock.l_pid > 0 ? lock.l_pid : nil)
    }
}
