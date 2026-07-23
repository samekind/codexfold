import Darwin
import Dispatch
import Foundation

private enum TestFailure: Error, CustomStringConvertible {
    case failed(String)

    var description: String {
        switch self {
        case .failed(let message): return message
        }
    }
}

private func require(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() {
        throw TestFailure.failed(message)
    }
}

private typealias ShmOpenFunction = @convention(c) (
    UnsafePointer<CChar>,
    Int32,
    mode_t
) -> Int32

private func makePOSIXSharedMemoryDescriptor(capacity: Int) throws -> Int32 {
    guard capacity > 0 else { throw POSIXError(.EINVAL) }
    guard let library = Darwin.dlopen(nil, RTLD_NOW) else { throw POSIXError(.ENOENT) }
    defer { _ = Darwin.dlclose(library) }
    guard let symbol = Darwin.dlsym(library, "shm_open") else { throw POSIXError(.ENOSYS) }
    let openSharedMemory = unsafeBitCast(symbol, to: ShmOpenFunction.self)
    let name = String(format: "/cfs-test-%08x", Darwin.arc4random())
    let descriptor = name.withCString {
        openSharedMemory($0, O_RDWR | O_CREAT | O_EXCL, mode_t(S_IRUSR | S_IWUSR))
    }
    guard descriptor >= 0 else {
        throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
    guard name.withCString({ Darwin.shm_unlink($0) }) == 0 else {
        let code = errno
        Darwin.close(descriptor)
        throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
    }
    let pageSize = Int(Darwin.getpagesize())
    let mappedLength = ((capacity + pageSize - 1) / pageSize) * pageSize
    guard Darwin.ftruncate(descriptor, off_t(mappedLength)) == 0 else {
        let code = errno
        Darwin.close(descriptor)
        throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
    }
    return descriptor
}

private func populateMappedDescriptor(_ descriptor: Int32, bytes: [UInt8]) throws {
    let capacity = bytes.count
    let pageSize = Int(Darwin.getpagesize())
    let mappedLength = ((capacity + pageSize - 1) / pageSize) * pageSize
    let writable = Darwin.mmap(nil, mappedLength, PROT_READ | PROT_WRITE, MAP_SHARED, descriptor, 0)
    guard writable != MAP_FAILED, let writable else {
        throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
    defer { _ = Darwin.munmap(writable, mappedLength) }
    _ = bytes.withUnsafeBytes { source in
        Darwin.memcpy(writable, source.baseAddress!, capacity)
    }
}

private func makeWindow(bytes: [UInt8]) throws -> WireSharedReadWindow {
    let capacity = bytes.count
    let descriptor = try makePOSIXSharedMemoryDescriptor(capacity: bytes.count)
    do {
        try populateMappedDescriptor(descriptor, bytes: bytes)
    } catch {
        Darwin.close(descriptor)
        throw error
    }
    return try WireSharedReadWindow(descriptor: descriptor, capacity: capacity)
}

private func makeRegularFileDescriptor(bytes: [UInt8]) throws -> Int32 {
    let capacity = bytes.count
    let pageSize = Int(Darwin.getpagesize())
    let mappedLength = ((capacity + pageSize - 1) / pageSize) * pageSize
    var template = Array("/private/tmp/codexfold-read-cache.XXXXXX".utf8CString)
    let descriptor = template.withUnsafeMutableBufferPointer { buffer in
        Darwin.mkstemp(buffer.baseAddress!)
    }
    guard descriptor >= 0 else {
        throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
    }
    let path = String(cString: template)
    _ = path.withCString { Darwin.unlink($0) }
    guard Darwin.ftruncate(descriptor, off_t(mappedLength)) == 0 else {
        let code = errno
        Darwin.close(descriptor)
        throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
    }
    var completed = 0
    while completed < capacity {
        let amount = bytes.withUnsafeBytes { source in
            Darwin.pwrite(
                descriptor,
                source.baseAddress!.advanced(by: completed),
                capacity - completed,
                off_t(completed)
            )
        }
        guard amount > 0 else {
            let code = errno
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
        }
        completed += amount
    }
    return descriptor
}

private func makeRegularFileWindow(bytes: [UInt8]) throws -> WireSharedReadWindow {
    try WireSharedReadWindow(
        fileDescriptor: makeRegularFileDescriptor(bytes: bytes),
        capacity: bytes.count
    )
}

private func makeWireEntry(
    path: String = "/sessions/example.jsonl",
    nodeID: UInt64 = 17,
    parentID: UInt64 = 9,
    type: WireEntryType = .file,
    size: UInt64 = 4096,
    allocSize: UInt64 = 4096,
    modifyTime: timespec = timespec(tv_sec: 100, tv_nsec: 10),
    changeTime: timespec = timespec(tv_sec: 101, tv_nsec: 20),
    accessTime: timespec = timespec(tv_sec: 102, tv_nsec: 30),
    namespaceID: UInt64 = 1,
    contentGeneration: UInt64 = 0
) throws -> WireEntry {
    var writer = WireWriter()
    writer.string(path)
    writer.string((path as NSString).lastPathComponent)
    writer.uint64(nodeID)
    writer.uint64(parentID)
    writer.uint8(type.rawValue)
    writer.uint32(0o600)
    writer.uint32(501)
    writer.uint32(20)
    writer.uint64(size)
    writer.uint64(allocSize)
    writer.time(modifyTime)
    writer.time(changeTime)
    writer.time(accessTime)
    writer.uint64(namespaceID)
    writer.uint64(contentGeneration)
    var reader = WireReader(writer.data)
    let entry = try WireEntry(reader: &reader, includesContentGeneration: true)
    try reader.finish()
    return entry
}

private func testPOSIXSharedMemoryRejectsPreadButSupportsMapping() throws {
    let bytes = (0..<4096).map { UInt8(truncatingIfNeeded: $0 * 19) }
    let descriptor = try makePOSIXSharedMemoryDescriptor(capacity: bytes.count)
    defer { Darwin.close(descriptor) }
    try populateMappedDescriptor(descriptor, bytes: bytes)
    var byte: UInt8 = 0
    errno = 0
    let amount = Darwin.pread(descriptor, &byte, 1, 0)
    try require(amount == -1, "POSIX shared memory unexpectedly accepted pread")
    try require(errno == ESPIPE || errno == EIO, "POSIX shared memory pread failed with unexpected errno \(errno)")

    let duplicate = Darwin.dup(descriptor)
    guard duplicate >= 0 else { throw POSIXError(.EIO) }
    let window = try WireSharedReadWindow(descriptor: duplicate, capacity: bytes.count)
    let copied = try window.copyData(count: bytes.count)
    try require(copied == Data(bytes), "mapped POSIX shared memory bytes changed")
}

private func testRegularFileWindowSupportsConcurrentMappedCopies() throws {
    let bytes = (0..<(4 * 1024 * 1024)).map { UInt8(truncatingIfNeeded: $0 * 23) }
    let window = try makeRegularFileWindow(bytes: bytes)
    let workers = DispatchGroup()
    let failureLock = NSLock()
    var failures: [String] = []

    for worker in 0..<8 {
        workers.enter()
        DispatchQueue.global(qos: .userInitiated).async {
            let offset = worker * 384 * 1024
            var copied = [UInt8](repeating: 0, count: 512 * 1024)
            let copiedCount = copied.count
            let success = copied.withUnsafeMutableBytes { destination in
                window.copyBytes(from: offset, count: copiedCount, to: destination.baseAddress!)
            }
            if !success || copied != Array(bytes[offset..<(offset + copied.count)]) {
                failureLock.withLock { failures.append("regular-file worker \(worker) observed corrupt bytes") }
            }
            workers.leave()
        }
    }
    workers.wait()
    try require(failureLock.withLock { failures.isEmpty }, failures.joined(separator: "; "))
}

private func testRegularFileWindowOwnsDescriptorLifetime() throws {
    let bytes = Array("descriptor-lifetime".utf8)
    let descriptor = try makeRegularFileDescriptor(bytes: bytes)
    var window: WireSharedReadWindow? = try WireSharedReadWindow(
        fileDescriptor: descriptor,
        capacity: bytes.count
    )
    try require(window?.capacity == bytes.count, "regular-file window lost its capacity")
    try require(Darwin.fcntl(descriptor, F_GETFD) >= 0, "regular-file descriptor closed during window lifetime")
    window = nil
    errno = 0
    try require(Darwin.fcntl(descriptor, F_GETFD) == -1 && errno == EBADF, "regular-file descriptor leaked after window release")
}

private func testRegularFileWindowMappedCopyThroughput() throws {
    let windowBytes = 28 * 1024 * 1024
    let readBytes = 4 * 1024 * 1024
    let totalBytes = 256 * 1024 * 1024
    let bytes = (0..<windowBytes).map { UInt8(truncatingIfNeeded: $0 * 29) }
    let window = try makeRegularFileWindow(bytes: bytes)
    var destination = [UInt8](repeating: 0, count: readBytes)
    let started = DispatchTime.now().uptimeNanoseconds
    for index in 0..<(totalBytes / readBytes) {
        let sourceOffset = (index % (windowBytes / readBytes)) * readBytes
        let copied = destination.withUnsafeMutableBytes { output in
            window.copyBytes(from: sourceOffset, count: readBytes, to: output.baseAddress!)
        }
        try require(copied, "mapped throughput copy failed at chunk \(index)")
    }
    let elapsed = DispatchTime.now().uptimeNanoseconds - started
    let throughput = Double(totalBytes) / (Double(elapsed) / 1_000_000_000)
    try require(throughput >= Double(4 * 1024 * 1024 * 1024), "mapped copy throughput \(throughput) B/s is below 4 GiB/s")
}

private func testSharedWindowCopiesExactRange() throws {
    let bytes = (0..<4096).map { UInt8(truncatingIfNeeded: $0 * 17) }
    let window = try makeWindow(bytes: bytes)
    var releases = 0
    let block = try CodexFoldCachedReadBlock(
        sharedWindow: window,
        count: bytes.count,
        release: { releases += 1 }
    )
    var copied = [UInt8](repeating: 0, count: 777)
    let copiedCount = copied.count
    let success = copied.withUnsafeMutableBytes { destination in
        block.copyBytes(from: 901, count: copiedCount, to: destination.baseAddress!)
    }
    try require(success, "shared-window copy was rejected")
    try require(copied == Array(bytes[901..<(901 + copied.count)]), "shared-window bytes changed")
    let rejected = copied.withUnsafeMutableBytes { destination in
        block.copyBytes(from: bytes.count - 1, count: 2, to: destination.baseAddress!)
    }
    try require(!rejected, "out-of-range copy succeeded")
    try require(releases == 0, "lease released while block remained alive")
}

private func testReadAheadPolicyKeepsEightBlockHorizonWithEightWorkers() throws {
    let policy = CodexFoldReadAheadPolicy(negotiatedReadBytes: 32 * 1024 * 1024)
    try require(policy.readAheadBytes == 12 * 1024 * 1024, "read-ahead blocks must align with three 4 MiB reads")
    try require(policy.concurrentPrefetchCount == 8, "prefetch concurrency must remain eight")
    try require(policy.scheduledPrefetchCount == 8, "prefetch horizon must stay eight blocks ahead")
    try require(policy.maxCachedBlocks == 9, "cache must retain the current block plus the eight-block horizon")
}

private func testNamespaceRefreshRetainsOnlyUnchangedFileData() throws {
    let original = try makeWireEntry()
    let newNamespaceAndAccessTime = try makeWireEntry(
        accessTime: timespec(tv_sec: 999, tv_nsec: 40),
        namespaceID: 2
    )
    try require(
        original.hasSameCachedFileData(as: newNamespaceAndAccessTime),
        "namespace or access-time changes evicted unchanged file data"
    )
    let resized = try makeWireEntry(size: 4097, allocSize: 8192)
    let modified = try makeWireEntry(modifyTime: timespec(tv_sec: 103, tv_nsec: 10))
    let changed = try makeWireEntry(changeTime: timespec(tv_sec: 104, tv_nsec: 20))
    let moved = try makeWireEntry(path: "/sessions/moved.jsonl")
    let replaced = try makeWireEntry(nodeID: 18)
    try require(
        !original.hasSameCachedFileData(as: resized),
        "size changes retained stale file data"
    )
    try require(
        !original.hasSameCachedFileData(as: modified),
        "mtime changes retained stale file data"
    )
    try require(
        !original.hasSameCachedFileData(as: changed),
        "ctime changes retained stale file data"
    )
    try require(
        !original.hasSameCachedFileData(as: moved),
        "path changes retained stale file data"
    )
    try require(
        !original.hasSameCachedFileData(as: replaced),
        "node changes retained stale file data"
    )
    let directory = try makeWireEntry(path: "/sessions", type: .directory)
    try require(
        !directory.hasSameCachedFileData(as: directory),
        "directory was treated as cached file data"
    )
    try require(
        directory.hasSameObjectIdentity(as: directory),
        "unchanged directory identity was discarded"
    )
    try require(
        directory.hasSameCachedDirectoryContents(as: directory),
        "unchanged directory contents were discarded"
    )
    let changedDirectory = try makeWireEntry(
        path: "/sessions",
        type: .directory,
        modifyTime: timespec(tv_sec: 103, tv_nsec: 10)
    )
    try require(
        !directory.hasSameCachedDirectoryContents(as: changedDirectory),
        "changed directory contents retained a stale name cache"
    )
    let movedParent = try makeWireEntry(parentID: 99)
    try require(
        original.hasSameObjectIdentity(as: movedParent),
        "a parent directory generation changed the child's own identity"
    )
    try require(
        original.hasSameCachedFileData(as: movedParent),
        "a parent directory generation evicted unchanged child data"
    )
}

private func testNormalizedWriteInvalidatesOnlyWhenVisibleLayoutDiverges() throws {
    try require(
        !writeRequiresKernelCacheInvalidation(
            previousSize: 0,
            offset: 0,
            writtenBytes: 13,
            visibleSize: 13
        ),
        "ordinary initial write invalidated the kernel cache"
    )
    try require(
        !writeRequiresKernelCacheInvalidation(
            previousSize: 13,
            offset: 0,
            writtenBytes: 26,
            visibleSize: 26
        ),
        "first full-page append snapshot invalidated matching cache data"
    )
    try require(
        writeRequiresKernelCacheInvalidation(
            previousSize: 26,
            offset: 0,
            writtenBytes: 26,
            visibleSize: 39
        ),
        "normalized append failed to invalidate a shorter kernel snapshot"
    )
    try require(
        !writeRequiresKernelCacheInvalidation(
            previousSize: 1024,
            offset: 512,
            writtenBytes: 128,
            visibleSize: 1024
        ),
        "in-place overwrite invalidated an unchanged visible layout"
    )
}

private func testEvictionWaitsForLastReader() throws {
    let bytes = (0..<(2 * 1024 * 1024)).map { UInt8(truncatingIfNeeded: $0 * 31) }
    let window = try makeWindow(bytes: bytes)
    let releaseLock = NSLock()
    var releases = 0
    var cache: [Int64: CodexFoldCachedReadBlock] = [:]
    cache[0] = try CodexFoldCachedReadBlock(
        sharedWindow: window,
        count: bytes.count,
        release: {
            releaseLock.withLock { releases += 1 }
        }
    )
    var reader = cache[0]
    cache.removeAll()
    try require(releaseLock.withLock { releases == 0 }, "eviction released an active reader")

    var copied = [UInt8](repeating: 0, count: 1024 * 1024)
    let copiedCount = copied.count
    let success = copied.withUnsafeMutableBytes { destination in
        reader!.copyBytes(from: 512 * 1024, count: copiedCount, to: destination.baseAddress!)
    }
    try require(success, "evicted block could not finish its active read")
    try require(copied == Array(bytes[(512 * 1024)..<(1536 * 1024)]), "evicted block bytes changed")
    reader = nil
    try require(releaseLock.withLock { releases == 1 }, "lease did not release after the final reader")
}

private func testConcurrentReadersRetainLease() throws {
    let bytes = (0..<(4 * 1024 * 1024)).map { UInt8(truncatingIfNeeded: $0 * 13) }
    let window = try makeWindow(bytes: bytes)
    let releaseLock = NSLock()
    var releases = 0
    var block: CodexFoldCachedReadBlock? = try CodexFoldCachedReadBlock(
        sharedWindow: window,
        count: bytes.count,
        release: {
            releaseLock.withLock { releases += 1 }
        }
    )
    let start = DispatchSemaphore(value: 0)
    let ready = DispatchGroup()
    let workers = DispatchGroup()
    let failureLock = NSLock()
    var failures: [String] = []

    for worker in 0..<8 {
        let retained = block!
        ready.enter()
        workers.enter()
        DispatchQueue.global(qos: .userInitiated).async {
            ready.leave()
            start.wait()
            let offset = worker * 256 * 1024
            var copied = [UInt8](repeating: 0, count: 512 * 1024)
            let copiedCount = copied.count
            let success = copied.withUnsafeMutableBytes { destination in
                retained.copyBytes(from: offset, count: copiedCount, to: destination.baseAddress!)
            }
            if !success || copied != Array(bytes[offset..<(offset + copied.count)]) {
                failureLock.withLock { failures.append("worker \(worker) observed corrupt bytes") }
            }
            workers.leave()
        }
    }
    ready.wait()
    block = nil
    try require(releaseLock.withLock { releases == 0 }, "lease released before concurrent readers started")
    for _ in 0..<8 { start.signal() }
    workers.wait()
    try require(failureLock.withLock { failures.isEmpty }, failures.joined(separator: "; "))
    try require(releaseLock.withLock { releases == 1 }, "concurrent readers did not release exactly once")
}

@main
private struct ReadCacheTests {
    static func main() throws {
        try testPOSIXSharedMemoryRejectsPreadButSupportsMapping()
        try testRegularFileWindowSupportsConcurrentMappedCopies()
        try testRegularFileWindowOwnsDescriptorLifetime()
        try testRegularFileWindowMappedCopyThroughput()
        try testSharedWindowCopiesExactRange()
        try testReadAheadPolicyKeepsEightBlockHorizonWithEightWorkers()
        try testNamespaceRefreshRetainsOnlyUnchangedFileData()
        try testNormalizedWriteInvalidatesOnlyWhenVisibleLayoutDiverges()
        try testEvictionWaitsForLastReader()
        try testConcurrentReadersRetainLease()
        print("ReadCacheTests: PASS")
    }
}
