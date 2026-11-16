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

// A minimal daemon on a Unix socket, speaking just enough of the wire protocol
// to answer hello and ping, so a test can control when and what it replies.
private let testWireMagic = Data([0x43, 0x46, 0x53, 0x50])
private let testDescriptorMagic = Data([0x43, 0x46, 0x53, 0x52])
private let testWireVersion: UInt16 = 2
private let testGeneration: UInt64 = 7

private struct TestWireRequest {
    let operation: UInt8
    let requestID: UInt64
}

private func readAll(_ descriptor: Int32, count: Int) -> Data? {
    var result = Data(count: count)
    var completed = 0
    while completed < count {
        let amount = result.withUnsafeMutableBytes { bytes in
            Darwin.read(descriptor, bytes.baseAddress!.advanced(by: completed), count - completed)
        }
        if amount < 0 && errno == EINTR { continue }
        guard amount > 0 else { return nil }
        completed += amount
    }
    return result
}

private func readTestRequest(_ descriptor: Int32) -> TestWireRequest? {
    guard let header = readAll(descriptor, count: 40) else { return nil }
    var reader = WireReader(header)
    guard (try? reader.raw(count: 4)) == testWireMagic,
          (try? reader.uint16()) == testWireVersion,
          (try? reader.uint8()) == 1,
          let operation = try? reader.uint8(),
          (try? reader.uint32()) != nil,
          let requestID = try? reader.uint64(),
          (try? reader.uint64()) != nil,
          (try? reader.uint32()) != nil,
          let length = try? reader.uint32() else {
        return nil
    }
    if length > 0, readAll(descriptor, count: Int(length)) == nil {
        return nil
    }
    return TestWireRequest(operation: operation, requestID: requestID)
}

private func writeTestResponse(_ descriptor: Int32, operation: UInt8, requestID: UInt64, status: UInt32 = 0, payload: Data = Data()) {
    var writer = WireWriter()
    writer.raw(testWireMagic)
    writer.uint16(testWireVersion)
    writer.uint8(2)
    writer.uint8(operation)
    writer.uint32(0)
    writer.uint64(requestID)
    writer.uint64(testGeneration)
    writer.uint32(status)
    writer.uint32(UInt32(payload.count))
    writer.uint32(0)
    writer.raw(payload)
    let data = writer.data
    _ = data.withUnsafeBytes { bytes in Darwin.write(descriptor, bytes.baseAddress!, data.count) }
}

private func answerTestHello(_ descriptor: Int32) -> Bool {
    guard let hello = readTestRequest(descriptor), hello.operation == WireOperation.hello.rawValue else {
        return false
    }
    var payload = WireWriter()
    payload.uint32(1 << 20)
    payload.uint64(1)
    payload.uint32(0)
    writeTestResponse(descriptor, operation: hello.operation, requestID: hello.requestID, payload: payload.data)
    return true
}

// Runs one connection of a fake daemon. The handler gets the accepted socket
// after hello has been answered. Returns the descriptor the client should use.
private func startTestDaemon(_ handler: @escaping (Int32) -> Void) throws -> (WireDescriptor, () -> Void) {
    let path = NSTemporaryDirectory() + "cfw-\(getpid())-\(UInt32.random(in: 0...UInt32.max)).sock"
    let listener = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
    try require(listener >= 0, "test daemon socket failed")
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    let pathBytes = Array(path.utf8CString)
    try require(pathBytes.count <= MemoryLayout.size(ofValue: address.sun_path), "test socket path too long")
    withUnsafeMutableBytes(of: &address.sun_path) { destination in
        for (index, value) in pathBytes.enumerated() { destination[index] = UInt8(bitPattern: value) }
    }
    let bound = withUnsafePointer(to: &address) { pointer in
        pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            Darwin.bind(listener, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    try require(bound == 0 && Darwin.listen(listener, 1) == 0, "test daemon bind failed")
    let finished = DispatchSemaphore(value: 0)
    Thread {
        let connection = Darwin.accept(listener, nil, nil)
        if connection >= 0 {
            var noSigPipe: Int32 = 1
            _ = setsockopt(connection, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
            if answerTestHello(connection) { handler(connection) }
            Darwin.close(connection)
        }
        finished.signal()
    }.start()
    var descriptor = WireWriter()
    descriptor.raw(testDescriptorMagic)
    descriptor.uint16(testWireVersion)
    descriptor.uint16(0)
    descriptor.uint64(testGeneration)
    descriptor.string(path)
    descriptor.bytes(Data(repeating: 0x5A, count: 16))
    let stop = {
        _ = finished.wait(timeout: .now() + 5)
        Darwin.close(listener)
        unlink(path)
    }
    return (try WireDescriptor(data: descriptor.data), stop)
}

private func posixCode(_ body: () throws -> Void) -> POSIXErrorCode? {
    do {
        try body()
        return nil
    } catch {
        return (error as? POSIXError)?.code
    }
}

// A reply that arrives after its request timed out must not be read as the
// answer to the next request. Before, every later request on the connection
// failed its ID check with EPROTO, which recovery does not handle, so the
// mount answered "Protocol error" to everything until the daemon restarted.
private func testLateReplyDoesNotPoisonTheConnection() throws {
    let (descriptor, stop) = try startTestDaemon { connection in
        guard let ping = readTestRequest(connection) else { return }
        Thread.sleep(forTimeInterval: 0.4)
        writeTestResponse(connection, operation: ping.operation, requestID: ping.requestID)
        while readTestRequest(connection) != nil {}
    }
    defer { stop() }
    let connection = try WireConnection(descriptor: descriptor, idleTimeout: 0.15)
    defer { connection.close() }
    try require(posixCode { _ = try connection.request(.ping) } == .ETIMEDOUT, "slow reply did not time out")
    Thread.sleep(forTimeInterval: 0.5)
    let next = posixCode { _ = try connection.request(.ping) }
    try require(next != .EPROTO, "late reply poisoned the next request")
    try require(next.map { isWireTransportError(POSIXError($0)) } == true, "request after a timeout did not fail as a transport error: \(String(describing: next))")
}

private func testMismatchedReplyIsATransportFailure() throws {
    let (descriptor, stop) = try startTestDaemon { connection in
        guard let ping = readTestRequest(connection) else { return }
        writeTestResponse(connection, operation: ping.operation, requestID: ping.requestID &+ 5)
        while readTestRequest(connection) != nil {}
    }
    defer { stop() }
    let connection = try WireConnection(descriptor: descriptor, idleTimeout: 1)
    defer { connection.close() }
    try require(posixCode { _ = try connection.request(.ping) } == .ECONNRESET, "mismatched reply was not reported as a reset connection")
    try require(posixCode { _ = try connection.request(.ping) } == .ENOTCONN, "desynchronized connection stayed in use")
}

private func testDaemonErrorKeepsTheConnection() throws {
    let (descriptor, stop) = try startTestDaemon { connection in
        guard let first = readTestRequest(connection) else { return }
        writeTestResponse(connection, operation: first.operation, requestID: first.requestID, status: UInt32(ENOENT))
        guard let second = readTestRequest(connection) else { return }
        writeTestResponse(connection, operation: second.operation, requestID: second.requestID)
    }
    defer { stop() }
    let connection = try WireConnection(descriptor: descriptor, idleTimeout: 1)
    defer { connection.close() }
    try require(posixCode { _ = try connection.request(.ping) } == .ENOENT, "daemon status was not reported")
    try require(posixCode { _ = try connection.request(.ping) } == nil, "a daemon error closed a healthy connection")
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
        try testLateReplyDoesNotPoisonTheConnection()
        try testMismatchedReplyIsATransportFailure()
        try testDaemonErrorKeepsTheConnection()
        print("ReadCacheTests: PASS")
    }
}
