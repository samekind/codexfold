import Darwin
import Foundation

enum DurableFilePublicationStep: Equatable {
    case createTemporary
    case write
    case syncFile
    case rename
    case verifyDestination
    case syncDirectory
}

enum DurableAppGroupFile {
    typealias StepObserver = (DurableFilePublicationStep) -> Void
    typealias FailureInjector = (DurableFilePublicationStep) -> POSIXErrorCode?

    static func write(
        _ data: Data,
        to destination: URL,
        observe: StepObserver? = nil,
        failBefore: FailureInjector? = nil
    ) throws {
        let directory = destination.deletingLastPathComponent()
        let name = destination.lastPathComponent
        guard !name.isEmpty, name != ".", name != "..", !name.contains("/") else {
            throw POSIXError(.EINVAL)
        }

        let directoryDescriptor = directory.path.withCString {
            Darwin.open($0, O_RDONLY | O_CLOEXEC | O_DIRECTORY | O_NOFOLLOW)
        }
        guard directoryDescriptor >= 0 else { throw currentPOSIXError() }
        defer { Darwin.close(directoryDescriptor) }

        try rejectUnsafeDestination(name, directoryDescriptor: directoryDescriptor)

        let temporaryName = ".(name).(UUID().uuidString.lowercased()).tmp"
        try failIfRequested(.createTemporary, failBefore: failBefore)
        let descriptor = temporaryName.withCString {
            Darwin.openat(
                directoryDescriptor,
                $0,
                O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW,
                S_IRUSR | S_IWUSR
            )
        }
        guard descriptor >= 0 else { throw currentPOSIXError() }
        observe?(.createTemporary)

        var temporaryExists = true
        defer {
            Darwin.close(descriptor)
            if temporaryExists {
                temporaryName.withCString { _ = Darwin.unlinkat(directoryDescriptor, $0, 0) }
            }
        }

        try failIfRequested(.write, failBefore: failBefore)
        try writeAll(data, descriptor: descriptor)
        observe?(.write)

        try failIfRequested(.syncFile, failBefore: failBefore)
        guard Darwin.fsync(descriptor) == 0 else { throw currentPOSIXError() }
        guard Darwin.fcntl(descriptor, F_FULLFSYNC) == 0 else { throw currentPOSIXError() }
        observe?(.syncFile)

        var temporaryInfo = stat()
        guard Darwin.fstat(descriptor, &temporaryInfo) == 0,
              (temporaryInfo.st_mode & S_IFMT) == S_IFREG,
              temporaryInfo.st_size == Int64(data.count) else {
            throw POSIXError(.EIO)
        }

        try failIfRequested(.rename, failBefore: failBefore)
        let renameResult = temporaryName.withCString { source in
            name.withCString { target in
                Darwin.renameat(directoryDescriptor, source, directoryDescriptor, target)
            }
        }
        guard renameResult == 0 else { throw currentPOSIXError() }
        temporaryExists = false
        observe?(.rename)

        try failIfRequested(.verifyDestination, failBefore: failBefore)
        let publishedDescriptor = name.withCString {
            Darwin.openat(directoryDescriptor, $0, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK)
        }
        guard publishedDescriptor >= 0 else { throw currentPOSIXError() }
        defer { Darwin.close(publishedDescriptor) }
        var publishedInfo = stat()
        guard Darwin.fstat(publishedDescriptor, &publishedInfo) == 0,
              (publishedInfo.st_mode & S_IFMT) == S_IFREG,
              publishedInfo.st_dev == temporaryInfo.st_dev,
              publishedInfo.st_ino == temporaryInfo.st_ino,
              publishedInfo.st_size == temporaryInfo.st_size else {
            throw POSIXError(.EIO)
        }
        observe?(.verifyDestination)

        try failIfRequested(.syncDirectory, failBefore: failBefore)
        guard Darwin.fsync(directoryDescriptor) == 0 else { throw currentPOSIXError() }
        observe?(.syncDirectory)
    }

    static func read(_ source: URL, maximumBytes: Int = 1_048_576) throws -> Data {
        guard maximumBytes >= 0 else { throw POSIXError(.EINVAL) }
        let directory = source.deletingLastPathComponent()
        let name = source.lastPathComponent
        guard !name.isEmpty, name != ".", name != "..", !name.contains("/") else {
            throw POSIXError(.EINVAL)
        }
        let directoryDescriptor = directory.path.withCString {
            Darwin.open($0, O_RDONLY | O_CLOEXEC | O_DIRECTORY | O_NOFOLLOW)
        }
        guard directoryDescriptor >= 0 else { throw currentPOSIXError() }
        defer { Darwin.close(directoryDescriptor) }

        let descriptor = name.withCString {
            Darwin.openat(directoryDescriptor, $0, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK)
        }
        guard descriptor >= 0 else { throw currentPOSIXError() }
        defer { Darwin.close(descriptor) }

        var before = stat()
        guard Darwin.fstat(descriptor, &before) == 0,
              (before.st_mode & S_IFMT) == S_IFREG,
              before.st_size >= 0,
              before.st_size <= Int64(maximumBytes) else {
            throw POSIXError(.EINVAL)
        }
        var data = Data(count: Int(before.st_size))
        try data.withUnsafeMutableBytes { rawBuffer in
            var offset = 0
            while offset < rawBuffer.count {
                let count = Darwin.read(
                    descriptor,
                    rawBuffer.baseAddress?.advanced(by: offset),
                    rawBuffer.count - offset
                )
                if count < 0 {
                    if errno == EINTR { continue }
                    throw currentPOSIXError()
                }
                if count == 0 { throw POSIXError(.EIO) }
                offset += count
            }
        }
        var after = stat()
        guard Darwin.fstat(descriptor, &after) == 0,
              (after.st_mode & S_IFMT) == S_IFREG,
              after.st_dev == before.st_dev,
              after.st_ino == before.st_ino,
              after.st_size == before.st_size,
              after.st_mtimespec.tv_sec == before.st_mtimespec.tv_sec,
              after.st_mtimespec.tv_nsec == before.st_mtimespec.tv_nsec,
              after.st_ctimespec.tv_sec == before.st_ctimespec.tv_sec,
              after.st_ctimespec.tv_nsec == before.st_ctimespec.tv_nsec else {
            throw POSIXError(.EIO)
        }
        return data
    }

    static func remove(_ destination: URL) throws {
        let directory = destination.deletingLastPathComponent()
        let name = destination.lastPathComponent
        guard !name.isEmpty, name != ".", name != "..", !name.contains("/") else {
            throw POSIXError(.EINVAL)
        }
        let directoryDescriptor = directory.path.withCString {
            Darwin.open($0, O_RDONLY | O_CLOEXEC | O_DIRECTORY | O_NOFOLLOW)
        }
        guard directoryDescriptor >= 0 else { throw currentPOSIXError() }
        defer { Darwin.close(directoryDescriptor) }

        var info = stat()
        let inspected = name.withCString {
            Darwin.fstatat(directoryDescriptor, $0, &info, AT_SYMLINK_NOFOLLOW)
        }
        if inspected != 0 {
            if errno == ENOENT { return }
            throw currentPOSIXError()
        }
        guard (info.st_mode & S_IFMT) == S_IFREG else { throw POSIXError(.EINVAL) }
        let removed = name.withCString { Darwin.unlinkat(directoryDescriptor, $0, 0) }
        guard removed == 0 else { throw currentPOSIXError() }
        guard Darwin.fsync(directoryDescriptor) == 0 else { throw currentPOSIXError() }
    }

    private static func rejectUnsafeDestination(
        _ name: String,
        directoryDescriptor: Int32
    ) throws {
        var info = stat()
        let result = name.withCString {
            Darwin.fstatat(directoryDescriptor, $0, &info, AT_SYMLINK_NOFOLLOW)
        }
        if result == 0 {
            guard (info.st_mode & S_IFMT) == S_IFREG else { throw POSIXError(.EINVAL) }
            return
        }
        guard errno == ENOENT else { throw currentPOSIXError() }
    }

    private static func writeAll(_ data: Data, descriptor: Int32) throws {
        try data.withUnsafeBytes { rawBuffer in
            var offset = 0
            while offset < rawBuffer.count {
                let count = Darwin.write(
                    descriptor,
                    rawBuffer.baseAddress?.advanced(by: offset),
                    rawBuffer.count - offset
                )
                if count < 0 {
                    if errno == EINTR { continue }
                    throw currentPOSIXError()
                }
                if count == 0 { throw POSIXError(.EIO) }
                offset += count
            }
        }
    }

    private static func failIfRequested(
        _ step: DurableFilePublicationStep,
        failBefore: FailureInjector?
    ) throws {
        if let code = failBefore?(step) { throw POSIXError(code) }
    }

    private static func currentPOSIXError() -> POSIXError {
        POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
}
