import Darwin
import Foundation

private let wireMagic = Data([0x43, 0x46, 0x53, 0x50])
private let descriptorMagic = Data([0x43, 0x46, 0x53, 0x52])
private let wireVersion: UInt16 = 2
private let wireHeaderSize = 40
private let defaultMaxPayload = 16 * 1024 * 1024

enum WireOperation: UInt8 {
    case hello = 1
    case ping
    case getattr
    case readDir
    case open
    case create
    case read
    case write
    case fsync
    case flush
    case release
    case truncate
    case mkdir
    case rename
    case unlink
    case rmdir
    case statfs
    case sync
    case namespaceVersion
    case setattr
    case getXattr
    case setXattr
    case listXattrs
}

enum WireEntryType: UInt8 {
    case unknown = 0
    case file
    case directory
    case symlink
}

struct WireDescriptor {
    let generation: UInt64
    let socketPath: String
    let token: Data

    init(resourceURL: URL) throws {
        var isDirectory = ObjCBool(false)
        guard FileManager.default.fileExists(atPath: resourceURL.path, isDirectory: &isDirectory) else {
            throw POSIXError(.ENOENT)
        }
        let descriptorURL = isDirectory.boolValue
            ? resourceURL.appendingPathComponent("descriptor.bin", isDirectory: false)
            : resourceURL
        try self.init(data: Data(contentsOf: descriptorURL))
    }

    init(data: Data) throws {
        var reader = WireReader(data)
        guard try reader.raw(count: 4) == descriptorMagic else {
            throw POSIXError(.EPROTO)
        }
        guard try reader.uint16() == wireVersion else {
            throw POSIXError(.EPROTONOSUPPORT)
        }
        _ = try reader.uint16()
        generation = try reader.uint64()
        socketPath = try reader.string(limit: 4096)
        token = try reader.bytes(limit: 256)
        try reader.finish()
        guard generation != 0, !socketPath.isEmpty, token.count >= 16 else {
            throw POSIXError(.EINVAL)
        }
    }
}

struct WireEntry {
    let path: String
    let name: String
    let nodeID: UInt64
    let parentID: UInt64
    let type: WireEntryType
    let mode: UInt32
    let uid: UInt32
    let gid: UInt32
    let size: UInt64
    let allocSize: UInt64
    let modifyTime: timespec
    let changeTime: timespec
    let accessTime: timespec
    let namespaceID: UInt64

    init(reader: inout WireReader) throws {
        path = try reader.string(limit: 1 << 20)
        name = try reader.string(limit: 4096)
        nodeID = try reader.uint64()
        parentID = try reader.uint64()
        guard let type = WireEntryType(rawValue: try reader.uint8()) else {
            throw POSIXError(.EPROTO)
        }
        self.type = type
        mode = try reader.uint32()
        uid = try reader.uint32()
        gid = try reader.uint32()
        size = try reader.uint64()
        allocSize = try reader.uint64()
        modifyTime = try reader.time()
        changeTime = try reader.time()
        accessTime = try reader.time()
        namespaceID = try reader.uint64()
    }
}

struct WireStatFS {
    let blockSize: UInt32
    let ioSize: UInt32
    let totalBytes: UInt64
    let availableBytes: UInt64
    let freeBytes: UInt64
    let usedBytes: UInt64
    let totalFiles: UInt64
    let freeFiles: UInt64

    init(reader: inout WireReader) throws {
        blockSize = try reader.uint32()
        ioSize = try reader.uint32()
        totalBytes = try reader.uint64()
        availableBytes = try reader.uint64()
        freeBytes = try reader.uint64()
        usedBytes = try reader.uint64()
        totalFiles = try reader.uint64()
        freeFiles = try reader.uint64()
    }
}

struct WireWriter {
    private(set) var data = Data()

    mutating func raw(_ value: Data) {
        data.append(value)
    }

    mutating func uint8(_ value: UInt8) {
        data.append(value)
    }

    mutating func uint16(_ value: UInt16) {
        appendFixed(value)
    }

    mutating func uint32(_ value: UInt32) {
        appendFixed(value)
    }

    mutating func uint64(_ value: UInt64) {
        appendFixed(value)
    }

    mutating func int64(_ value: Int64) {
        appendFixed(UInt64(bitPattern: value))
    }

    mutating func bytes(_ value: Data) {
        uint32(UInt32(value.count))
        raw(value)
    }

    mutating func string(_ value: String) {
        bytes(Data(value.utf8))
    }

    mutating func time(_ value: timespec) {
        int64(Int64(value.tv_sec))
        uint32(UInt32(value.tv_nsec))
    }

    private mutating func appendFixed<T: FixedWidthInteger>(_ value: T) {
        var littleEndian = value.littleEndian
        withUnsafeBytes(of: &littleEndian) { data.append(contentsOf: $0) }
    }
}

struct WireReader {
    private let data: Data
    private var offset = 0

    init(_ data: Data) {
        self.data = data
    }

    mutating func raw(count: Int) throws -> Data {
        guard count >= 0, offset <= data.count, data.count - offset >= count else {
            throw POSIXError(.EPROTO)
        }
        let result = data.subdata(in: offset..<(offset + count))
        offset += count
        return result
    }

    mutating func uint8() throws -> UInt8 {
        let value = try raw(count: 1)
        return value[value.startIndex]
    }

    mutating func uint16() throws -> UInt16 {
        try readFixed(UInt16.self)
    }

    mutating func uint32() throws -> UInt32 {
        try readFixed(UInt32.self)
    }

    mutating func uint64() throws -> UInt64 {
        try readFixed(UInt64.self)
    }

    mutating func int64() throws -> Int64 {
        Int64(bitPattern: try uint64())
    }

    mutating func bytes(limit: Int) throws -> Data {
        let count = Int(try uint32())
        guard count <= limit else {
            throw POSIXError(.E2BIG)
        }
        return try raw(count: count)
    }

    mutating func string(limit: Int) throws -> String {
        guard let value = String(data: try bytes(limit: limit), encoding: .utf8) else {
            throw POSIXError(.EILSEQ)
        }
        return value
    }

    mutating func time() throws -> timespec {
        let seconds = try int64()
        let nanoseconds = try uint32()
        guard nanoseconds < 1_000_000_000 else {
            throw POSIXError(.EPROTO)
        }
        return timespec(tv_sec: Int(seconds), tv_nsec: Int(nanoseconds))
    }

    mutating func finish() throws {
        guard offset == data.count else {
            throw POSIXError(.EPROTO)
        }
    }

    private mutating func readFixed<T: FixedWidthInteger>(_ type: T.Type) throws -> T {
        let value = try raw(count: MemoryLayout<T>.size)
        return value.withUnsafeBytes { bytes in
            T(littleEndian: bytes.loadUnaligned(as: T.self))
        }
    }
}

private struct WireFrame {
    let operation: WireOperation
    let requestID: UInt64
    let generation: UInt64
    let status: Int32
    let payload: Data
}

final class WireConnection {
    private let lock = NSLock()
    private var descriptor: WireDescriptor
    private var socket: Int32 = -1
    private var requestID: UInt64 = 1
    private var maxPayload = defaultMaxPayload

    init(descriptor: WireDescriptor) throws {
        self.descriptor = descriptor
        try connect()
        var hello = WireWriter()
        hello.bytes(descriptor.token)
        var reader = WireReader(try requestLocked(operation: .hello, generation: 0, payload: hello.data))
        maxPayload = Int(try reader.uint32())
        _ = try reader.uint64()
        try reader.finish()
        guard maxPayload >= 4096 else {
            throw POSIXError(.EPROTO)
        }
    }

    deinit {
        closeSocket()
    }

    func request(_ operation: WireOperation, payload: Data = Data()) throws -> Data {
        lock.lock()
        defer { lock.unlock() }
        return try requestLocked(operation: operation, generation: descriptor.generation, payload: payload)
    }

    func close() {
        lock.lock()
        closeSocket()
        lock.unlock()
    }

    private func connect() throws {
        let descriptor = Darwin.socket(AF_UNIX, SOCK_STREAM, 0)
        guard descriptor >= 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        var noSigPipe: Int32 = 1
        _ = setsockopt(descriptor, SOL_SOCKET, SO_NOSIGPIPE, &noSigPipe, socklen_t(MemoryLayout<Int32>.size))
        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(self.descriptor.socketPath.utf8CString)
        guard pathBytes.count <= MemoryLayout.size(ofValue: address.sun_path) else {
            Darwin.close(descriptor)
            throw POSIXError(.ENAMETOOLONG)
        }
        withUnsafeMutableBytes(of: &address.sun_path) { destination in
            destination.initializeMemory(as: UInt8.self, repeating: 0)
            for (index, value) in pathBytes.enumerated() {
                destination[index] = UInt8(bitPattern: value)
            }
        }
        let result = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { socketAddress in
                Darwin.connect(descriptor, socketAddress, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard result == 0 else {
            let code = errno
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
        }
        socket = descriptor
    }

    private func requestLocked(operation: WireOperation, generation: UInt64, payload: Data) throws -> Data {
        guard socket >= 0 else {
            throw POSIXError(.ENOTCONN)
        }
        guard payload.count <= maxPayload else {
            throw POSIXError(.E2BIG)
        }
        let currentID = requestID
        requestID &+= 1
        var header = WireWriter()
        header.raw(wireMagic)
        header.uint16(wireVersion)
        header.uint8(1)
        header.uint8(operation.rawValue)
        header.uint32(0)
        header.uint64(currentID)
        header.uint64(generation)
        header.uint32(0)
        header.uint32(UInt32(payload.count))
        header.uint32(0)
        try writeAll(header.data)
        try writeAll(payload)

        var responseReader = WireReader(try readExact(count: wireHeaderSize))
        guard try responseReader.raw(count: 4) == wireMagic,
              try responseReader.uint16() == wireVersion,
              try responseReader.uint8() == 2,
              try responseReader.uint8() == operation.rawValue else {
            throw POSIXError(.EPROTO)
        }
        _ = try responseReader.uint32()
        guard try responseReader.uint64() == currentID,
              try responseReader.uint64() == descriptor.generation else {
            throw POSIXError(.EPROTO)
        }
        let status = Int32(bitPattern: try responseReader.uint32())
        let payloadLength = Int(try responseReader.uint32())
        _ = try responseReader.uint32()
        try responseReader.finish()
        guard payloadLength <= maxPayload else {
            throw POSIXError(.E2BIG)
        }
        let responsePayload = try readExact(count: payloadLength)
        if status != 0 {
            throw POSIXError(POSIXErrorCode(rawValue: status) ?? .EIO)
        }
        return responsePayload
    }

    private func readExact(count: Int) throws -> Data {
        var result = Data(count: count)
        var completed = 0
        while completed < count {
            let amount = result.withUnsafeMutableBytes { bytes in
                Darwin.read(socket, bytes.baseAddress!.advanced(by: completed), count - completed)
            }
            if amount == 0 {
                throw POSIXError(.ECONNRESET)
            }
            if amount < 0 {
                if errno == EINTR { continue }
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
            completed += amount
        }
        return result
    }

    private func writeAll(_ data: Data) throws {
        var completed = 0
        while completed < data.count {
            let amount = data.withUnsafeBytes { bytes in
                Darwin.write(socket, bytes.baseAddress!.advanced(by: completed), data.count - completed)
            }
            if amount < 0 {
                if errno == EINTR { continue }
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
            guard amount > 0 else {
                throw POSIXError(.EIO)
            }
            completed += amount
        }
    }

    private func closeSocket() {
        if socket >= 0 {
            Darwin.close(socket)
            socket = -1
        }
    }
}

final class DaemonClient {
    let descriptor: WireDescriptor
    private let control: WireConnection

    init(descriptor: WireDescriptor) throws {
        self.descriptor = descriptor
        control = try WireConnection(descriptor: descriptor)
    }

    func newConnection() throws -> WireConnection {
        try WireConnection(descriptor: descriptor)
    }

    func ping() throws {
        _ = try control.request(.ping)
    }

    func getattr(_ path: String) throws -> WireEntry {
        var writer = WireWriter()
        writer.string(path)
        var reader = WireReader(try control.request(.getattr, payload: writer.data))
        let entry = try WireEntry(reader: &reader)
        try reader.finish()
        return entry
    }

    func readDir(_ path: String) throws -> [WireEntry] {
        var writer = WireWriter()
        writer.string(path)
        var reader = WireReader(try control.request(.readDir, payload: writer.data))
        let count = Int(try reader.uint32())
        var entries: [WireEntry] = []
        entries.reserveCapacity(count)
        for _ in 0..<count {
            entries.append(try WireEntry(reader: &reader))
        }
        try reader.finish()
        return entries
    }

    func open(_ path: String, flags: Int32, connection: WireConnection) throws -> UInt64 {
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(UInt32(bitPattern: flags))
        var reader = WireReader(try connection.request(.open, payload: writer.data))
        let handle = try reader.uint64()
        try reader.finish()
        return handle
    }

    func create(_ path: String, flags: Int32) throws -> (WireConnection, UInt64, WireEntry) {
        let connection = try newConnection()
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(UInt32(bitPattern: flags))
        var reader = WireReader(try connection.request(.create, payload: writer.data))
        let handle = try reader.uint64()
        let entry = try WireEntry(reader: &reader)
        try reader.finish()
        return (connection, handle, entry)
    }

    func read(handle: UInt64, offset: Int64, length: Int, connection: WireConnection) throws -> Data {
        var writer = WireWriter()
        writer.uint64(handle)
        writer.int64(offset)
        writer.uint32(UInt32(length))
        var reader = WireReader(try connection.request(.read, payload: writer.data))
        let data = try reader.bytes(limit: defaultMaxPayload)
        try reader.finish()
        return data
    }

    func write(handle: UInt64, offset: Int64, data: Data, connection: WireConnection) throws -> Int {
        var writer = WireWriter()
        writer.uint64(handle)
        writer.int64(offset)
        writer.bytes(data)
        var reader = WireReader(try connection.request(.write, payload: writer.data))
        let count = Int(try reader.uint32())
        try reader.finish()
        return count
    }

    func handleOperation(_ operation: WireOperation, handle: UInt64, connection: WireConnection) throws {
        var writer = WireWriter()
        writer.uint64(handle)
        _ = try connection.request(operation, payload: writer.data)
    }

    func truncate(_ path: String, size: UInt64) throws {
        guard size <= UInt64(Int64.max) else { throw POSIXError(.EFBIG) }
        var writer = WireWriter()
        writer.string(path)
        writer.int64(Int64(size))
        _ = try control.request(.truncate, payload: writer.data)
    }

    func mkdir(_ path: String, mode: UInt32) throws {
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(mode)
        _ = try control.request(.mkdir, payload: writer.data)
    }

    func rename(_ oldPath: String, _ newPath: String) throws {
        var writer = WireWriter()
        writer.string(oldPath)
        writer.string(newPath)
        _ = try control.request(.rename, payload: writer.data)
    }

    func remove(_ path: String, directory: Bool) throws {
        var writer = WireWriter()
        writer.string(path)
        _ = try control.request(directory ? .rmdir : .unlink, payload: writer.data)
    }

    func statfs() throws -> WireStatFS {
        var reader = WireReader(try control.request(.statfs))
        let result = try WireStatFS(reader: &reader)
        try reader.finish()
        return result
    }

    func sync() throws {
        _ = try control.request(.sync)
    }

    func namespaceVersion() throws -> UInt64 {
        var reader = WireReader(try control.request(.namespaceVersion))
        let version = try reader.uint64()
        try reader.finish()
        return version
    }

    func setAttributes(
        _ path: String,
        valid: UInt32,
        mode: UInt32,
        uid: UInt32,
        gid: UInt32,
        accessTime: timespec,
        modifyTime: timespec
    ) throws {
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(valid)
        writer.uint32(mode)
        writer.uint32(uid)
        writer.uint32(gid)
        writer.time(accessTime)
        writer.time(modifyTime)
        _ = try control.request(.setattr, payload: writer.data)
    }

    func getXattr(_ path: String, name: String) throws -> Data {
        var writer = WireWriter()
        writer.string(path)
        writer.string(name)
        var reader = WireReader(try control.request(.getXattr, payload: writer.data))
        let value = try reader.bytes(limit: defaultMaxPayload)
        try reader.finish()
        return value
    }

    func setXattr(_ path: String, name: String, value: Data, policy: UInt32) throws {
        var writer = WireWriter()
        writer.string(path)
        writer.string(name)
        writer.uint32(policy)
        writer.bytes(value)
        _ = try control.request(.setXattr, payload: writer.data)
    }

    func listXattrs(_ path: String) throws -> [String] {
        var writer = WireWriter()
        writer.string(path)
        var reader = WireReader(try control.request(.listXattrs, payload: writer.data))
        let count = Int(try reader.uint32())
        guard count <= defaultMaxPayload / 4 else {
            throw POSIXError(.E2BIG)
        }
        var attributes: [String] = []
        attributes.reserveCapacity(count)
        for _ in 0..<count {
            attributes.append(try reader.string(limit: 4096))
        }
        try reader.finish()
        return attributes
    }
}
