import Darwin
import Foundation

private let wireMagic = Data([0x43, 0x46, 0x53, 0x50])
private let descriptorMagic = Data([0x43, 0x46, 0x53, 0x52])
private let wireVersion: UInt16 = 2
private let wireHeaderSize = 40
private let defaultMaxPayload = 32 * 1024 * 1024
private let capabilityNativeReadFD: UInt32 = 1 << 0
private let capabilitySharedReadFD: UInt32 = 1 << 1
private let capabilitySharedWindow: UInt32 = 1 << 2
private let capabilitySharedFileWindow: UInt32 = 1 << 3
private let capabilityContentGeneration: UInt32 = 1 << 4
private let flagNativeReadFD: UInt32 = 1 << 0
private let flagSharedReadFD: UInt32 = 1 << 1
private let flagSharedWindow: UInt32 = 1 << 2
private let flagSharedFileWindow: UInt32 = 1 << 3
private let nativeReadFDMarker: UInt8 = 0x46
private let sharedReadFDMarker: UInt8 = 0x53
private let sharedWindowFDMarker: UInt8 = 0x57
private let sharedFileWindowFDMarker: UInt8 = 0x52
private let socketBufferBytes: Int32 = 4 * 1024 * 1024

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
    let contentGeneration: UInt64

    init(reader: inout WireReader, includesContentGeneration: Bool = false) throws {
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
        contentGeneration = includesContentGeneration ? try reader.uint64() : 0
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

    var remaining: Int {
        data.count - offset
    }

    mutating func raw(count: Int) throws -> Data {
        guard count >= 0, offset <= data.count, data.count - offset >= count else {
            throw POSIXError(.EPROTO)
        }
        let result = data[offset..<(offset + count)]
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

struct WireResponse {
    let operation: WireOperation
    let flags: UInt32
    let requestID: UInt64
    let generation: UInt64
    let status: Int32
    let payload: Data
    let nativeReadFD: Int32?
    let sharedReadFD: Int32?
    let sharedWindowFD: Int32?
}

struct WireOpenResult {
    let handle: UInt64
    let nativeReadFD: Int32?
    let sharedReadWindow: WireSharedReadWindow?
}

enum WireReadResult {
    case copied(Data)
    case sharedWindow(count: Int)
}

final class WireSharedReadWindow {
    let capacity: Int
    let wireFlag: UInt32

    private enum Storage {
        case mapped(UnsafeMutableRawPointer, Int)
        case mappedFile(UnsafeMutableRawPointer, Int, Int32)
    }

    private let storage: Storage

    init(descriptor: Int32, capacity: Int) throws {
        guard descriptor >= 0, capacity > 0 else {
            if descriptor >= 0 {
                Darwin.close(descriptor)
            }
            throw POSIXError(.EINVAL)
        }
        defer { Darwin.close(descriptor) }

        let pageSize = Int64(Darwin.getpagesize())
        let requestedBytes = Int64(capacity)
        let roundedBytes = ((requestedBytes + pageSize - 1) / pageSize) * pageSize
        guard roundedBytes > 0, roundedBytes <= Int64(Int.max) else {
            throw POSIXError(.EOVERFLOW)
        }
        var metadata = stat()
        guard Darwin.fstat(descriptor, &metadata) == 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        guard Int64(metadata.st_size) == roundedBytes else {
            throw POSIXError(.EPROTO)
        }
        let mappingLength = Int(roundedBytes)
        let mapped = Darwin.mmap(nil, mappingLength, PROT_READ, MAP_SHARED, descriptor, 0)
        let mapError = errno
        guard mapped != MAP_FAILED, let mapped else {
            throw POSIXError(POSIXErrorCode(rawValue: mapError) ?? .EIO)
        }
        Self.prefaultReadMapping(mapped, length: mappingLength)
        self.capacity = capacity
        self.wireFlag = flagSharedWindow
        self.storage = .mapped(mapped, mappingLength)
    }

    init(fileDescriptor descriptor: Int32, capacity: Int) throws {
        guard descriptor >= 0, capacity > 0 else {
            if descriptor >= 0 {
                Darwin.close(descriptor)
            }
            throw POSIXError(.EINVAL)
        }
        var metadata = stat()
        guard Darwin.fstat(descriptor, &metadata) == 0 else {
            let code = errno
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
        }
        let pageSize = Int64(Darwin.getpagesize())
        let requestedBytes = Int64(capacity)
        let roundedBytes = ((requestedBytes + pageSize - 1) / pageSize) * pageSize
        guard roundedBytes > 0,
              roundedBytes <= Int64(Int.max),
              Int64(metadata.st_size) == roundedBytes else {
            Darwin.close(descriptor)
            throw POSIXError(.EPROTO)
        }
        let mappingLength = Int(roundedBytes)
        let mapped = Darwin.mmap(nil, mappingLength, PROT_READ, MAP_SHARED, descriptor, 0)
        let mapError = errno
        guard mapped != MAP_FAILED, let mapped else {
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: mapError) ?? .EIO)
        }
        Self.prefaultReadMapping(mapped, length: mappingLength)
        self.capacity = capacity
        self.wireFlag = flagSharedFileWindow
        self.storage = .mappedFile(mapped, mappingLength, descriptor)
    }

    deinit {
        switch storage {
        case .mapped(let mapping, let mappingLength):
            _ = Darwin.munmap(mapping, mappingLength)
        case .mappedFile(let mapping, let mappingLength, let descriptor):
            _ = Darwin.munmap(mapping, mappingLength)
            Darwin.close(descriptor)
        }
    }

    func copyData(count: Int) throws -> Data {
        guard count >= 0, count <= capacity else {
            throw POSIXError(.EPROTO)
        }
        switch storage {
        case .mapped(let mapping, _):
            return Data(bytes: mapping, count: count)
        case .mappedFile(let mapping, _, _):
            return Data(bytes: mapping, count: count)
        }
    }

    func copyBytes(
        from sourceOffset: Int,
        count requestedCount: Int,
        to destination: UnsafeMutableRawPointer
    ) -> Bool {
        guard sourceOffset >= 0,
              requestedCount >= 0,
              sourceOffset <= capacity,
              requestedCount <= capacity - sourceOffset else {
            return false
        }
        guard requestedCount > 0 else { return true }
        switch storage {
        case .mapped(let mapping, _):
            Darwin.memcpy(
                destination,
                mapping.advanced(by: sourceOffset),
                requestedCount
            )
            return true
        case .mappedFile(let mapping, _, _):
            Darwin.memcpy(
                destination,
                mapping.advanced(by: sourceOffset),
                requestedCount
            )
            return true
        }
    }

    private static func prefaultReadMapping(_ mapping: UnsafeMutableRawPointer, length: Int) {
        guard length > 0 else { return }
        _ = Darwin.madvise(mapping, length, MADV_WILLNEED)
        if Darwin.mlock(mapping, length) == 0 {
            _ = Darwin.munlock(mapping, length)
        }
    }
}

final class WireConnection {
    private let lock = NSLock()
    private var descriptor: WireDescriptor
    private var socket: Int32 = -1
    private var requestID: UInt64 = 1
    private var maxPayload = defaultMaxPayload
    private(set) var supportsContentGeneration = false

    var maximumPayload: Int {
        maxPayload
    }

    var maximumReadBytes: Int {
        max(0, maxPayload - MemoryLayout<UInt32>.size)
    }

    init(descriptor: WireDescriptor) throws {
        self.descriptor = descriptor
        try connect()
        var hello = WireWriter()
        hello.bytes(descriptor.token)
        hello.uint32(
            capabilityNativeReadFD |
            capabilitySharedReadFD |
            capabilitySharedWindow |
            capabilitySharedFileWindow |
            capabilityContentGeneration
        )
        let response = try requestLocked(operation: .hello, generation: 0, payload: hello.data)
        if response.flags != 0 ||
            response.nativeReadFD != nil ||
            response.sharedReadFD != nil ||
            response.sharedWindowFD != nil {
            Self.closeDescriptors(response)
            throw POSIXError(.EPROTO)
        }
        var reader = WireReader(response.payload)
        maxPayload = Int(try reader.uint32())
        _ = try reader.uint64()
        let acceptedCapabilities = reader.remaining == 0 ? 0 : try reader.uint32()
        try reader.finish()
        supportsContentGeneration = acceptedCapabilities & capabilityContentGeneration != 0
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
        let response = try requestLocked(operation: operation, generation: descriptor.generation, payload: payload)
        guard response.flags == 0,
              response.nativeReadFD == nil,
              response.sharedReadFD == nil,
              response.sharedWindowFD == nil else {
            Self.closeDescriptors(response)
            throw POSIXError(.EPROTO)
        }
        return response.payload
    }

    func requestWithReadFD(_ operation: WireOperation, payload: Data = Data()) throws -> WireResponse {
        lock.lock()
        defer { lock.unlock() }
        return try requestLocked(operation: operation, generation: descriptor.generation, payload: payload)
    }

    func requestRead(
        payload: Data,
        requestedLength: Int,
        sharedWindow: WireSharedReadWindow?
    ) throws -> Data {
        let result = try requestReadResult(
            payload: payload,
            requestedLength: requestedLength,
            sharedWindow: sharedWindow,
            borrowSharedWindow: false
        )
        guard case .copied(let data) = result else {
            throw POSIXError(.EPROTO)
        }
        return data
    }

    func requestReadBorrowingSharedWindow(
        payload: Data,
        requestedLength: Int,
        sharedWindow: WireSharedReadWindow
    ) throws -> WireReadResult {
        try requestReadResult(
            payload: payload,
            requestedLength: requestedLength,
            sharedWindow: sharedWindow,
            borrowSharedWindow: true
        )
    }

    private func requestReadResult(
        payload: Data,
        requestedLength: Int,
        sharedWindow: WireSharedReadWindow?,
        borrowSharedWindow: Bool
    ) throws -> WireReadResult {
        guard requestedLength >= 0 else { throw POSIXError(.EINVAL) }
        lock.lock()
        defer { lock.unlock() }
        let response = try requestLocked(
            operation: .read,
            generation: descriptor.generation,
            payload: payload
        )
        let nativeReadFD = response.nativeReadFD
        var sharedReadFD = response.sharedReadFD
        let sharedWindowFD = response.sharedWindowFD
        do {
            guard nativeReadFD == nil, sharedWindowFD == nil else {
                throw POSIXError(.EPROTO)
            }
            if response.flags & (flagSharedWindow | flagSharedFileWindow) != 0 {
                guard let sharedWindow,
                      response.flags == sharedWindow.wireFlag,
                      sharedReadFD == nil,
                      response.flags == flagSharedWindow || response.flags == flagSharedFileWindow else {
                    throw POSIXError(.EPROTO)
                }
                var reader = WireReader(response.payload)
                let count = Int(try reader.uint32())
                try reader.finish()
                guard count <= requestedLength else {
                    throw POSIXError(.EPROTO)
                }
                if borrowSharedWindow {
                    return .sharedWindow(count: count)
                }
                // Keep the connection lock until the bytes are copied. A later
                // read on this handle may immediately overwrite the same window.
                return .copied(try sharedWindow.copyData(count: count))
            }
            if let descriptor = sharedReadFD {
                guard response.flags == flagSharedReadFD else {
                    throw POSIXError(.EPROTO)
                }
                sharedReadFD = nil
                defer { Darwin.close(descriptor) }
                var reader = WireReader(response.payload)
                let count = Int(try reader.uint32())
                try reader.finish()
                guard count > 0, count <= requestedLength else {
                    throw POSIXError(.EPROTO)
                }
                return .copied(try Self.mapSharedReadFD(
                    descriptor,
                    count: count,
                    maximumCount: requestedLength
                ))
            }
            guard response.flags == 0 else {
                throw POSIXError(.EPROTO)
            }
            var reader = WireReader(response.payload)
            let data = try reader.bytes(limit: maxPayload)
            try reader.finish()
            guard data.count <= requestedLength else {
                throw POSIXError(.EPROTO)
            }
            return .copied(data)
        } catch {
            if let nativeReadFD {
                Darwin.close(nativeReadFD)
            }
            if let sharedReadFD {
                Darwin.close(sharedReadFD)
            }
            if let sharedWindowFD {
                Darwin.close(sharedWindowFD)
            }
            throw error
        }
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
        var socketBuffer = socketBufferBytes
        _ = setsockopt(descriptor, SOL_SOCKET, SO_RCVBUF, &socketBuffer, socklen_t(MemoryLayout<Int32>.size))
        _ = setsockopt(descriptor, SOL_SOCKET, SO_SNDBUF, &socketBuffer, socklen_t(MemoryLayout<Int32>.size))
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

    private func requestLocked(operation: WireOperation, generation: UInt64, payload: Data) throws -> WireResponse {
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
        let flags = try responseReader.uint32()
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
        var nativeReadFD: Int32?
        var sharedReadFD: Int32?
        var sharedWindowFD: Int32?
        do {
            let knownFlags = flagNativeReadFD | flagSharedReadFD | flagSharedWindow | flagSharedFileWindow
            let transferFlags = flags & knownFlags
            guard flags & ~knownFlags == 0,
                  transferFlags.nonzeroBitCount <= 1 else {
                throw POSIXError(.EPROTO)
            }
            if flags & flagNativeReadFD != 0 {
                guard operation == .open, status == 0 else {
                    throw POSIXError(.EPROTO)
                }
                nativeReadFD = try readReadFD(expectedMarker: nativeReadFDMarker)
            }
            if flags & flagSharedReadFD != 0 {
                guard operation == .read, status == 0 else {
                    throw POSIXError(.EPROTO)
                }
                sharedReadFD = try readReadFD(expectedMarker: sharedReadFDMarker)
            }
            if flags & flagSharedWindow != 0 {
                guard status == 0 else {
                    throw POSIXError(.EPROTO)
                }
                if operation == .open {
                    sharedWindowFD = try readReadFD(expectedMarker: sharedWindowFDMarker)
                } else if operation != .read {
                    throw POSIXError(.EPROTO)
                }
            }
            if flags & flagSharedFileWindow != 0 {
                guard status == 0 else {
                    throw POSIXError(.EPROTO)
                }
                if operation == .open {
                    sharedWindowFD = try readReadFD(expectedMarker: sharedFileWindowFDMarker)
                } else if operation != .read {
                    throw POSIXError(.EPROTO)
                }
            }
        } catch {
            if let nativeReadFD { Darwin.close(nativeReadFD) }
            if let sharedReadFD { Darwin.close(sharedReadFD) }
            if let sharedWindowFD { Darwin.close(sharedWindowFD) }
            throw error
        }
        if status != 0 {
            if let nativeReadFD { Darwin.close(nativeReadFD) }
            if let sharedReadFD { Darwin.close(sharedReadFD) }
            if let sharedWindowFD { Darwin.close(sharedWindowFD) }
            throw POSIXError(POSIXErrorCode(rawValue: status) ?? .EIO)
        }
        return WireResponse(
            operation: operation,
            flags: flags,
            requestID: currentID,
            generation: descriptor.generation,
            status: status,
            payload: responsePayload,
            nativeReadFD: nativeReadFD,
            sharedReadFD: sharedReadFD,
            sharedWindowFD: sharedWindowFD
        )
    }

    private static func closeDescriptors(_ response: WireResponse) {
        if let nativeReadFD = response.nativeReadFD {
            Darwin.close(nativeReadFD)
        }
        if let sharedReadFD = response.sharedReadFD {
            Darwin.close(sharedReadFD)
        }
        if let sharedWindowFD = response.sharedWindowFD {
            Darwin.close(sharedWindowFD)
        }
    }

    private static func mapSharedReadFD(_ descriptor: Int32, count: Int, maximumCount: Int) throws -> Data {
        guard descriptor >= 0, count > 0, maximumCount >= count else {
            throw POSIXError(.EINVAL)
        }
        var metadata = stat()
        guard Darwin.fstat(descriptor, &metadata) == 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        let pageSize = Int64(Darwin.getpagesize())
        let mappedBytes = Int64(count)
        let mappingBytes = ((mappedBytes + pageSize - 1) / pageSize) * pageSize
        let maximumFileBytes = ((Int64(maximumCount) + pageSize - 1) / pageSize) * pageSize
        guard Int64(metadata.st_size) >= mappedBytes,
              Int64(metadata.st_size) <= maximumFileBytes else {
            throw POSIXError(.EPROTO)
        }
        let mappingLength = Int(mappingBytes)
        let mapping = Darwin.mmap(nil, mappingLength, PROT_READ, MAP_SHARED, descriptor, 0)
        let mapError = errno
        guard mapping != MAP_FAILED, let mapping else {
            throw POSIXError(POSIXErrorCode(rawValue: mapError) ?? .EIO)
        }
        return Data(bytesNoCopy: mapping, count: count, deallocator: .custom { pointer, _ in
            _ = Darwin.munmap(pointer, mappingLength)
        })
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

    private func readReadFD(expectedMarker: UInt8) throws -> Int32 {
        var marker: UInt8 = 0
        let headerSize = MemoryLayout<cmsghdr>.size
        let alignment = MemoryLayout<cmsghdr>.alignment
        let align: (Int) -> Int = { value in
            (value + alignment - 1) & ~(alignment - 1)
        }
        let dataOffset = align(headerSize)
        let controlSize = dataOffset + align(MemoryLayout<Int32>.size)
        var control = [UInt8](repeating: 0, count: controlSize)
        var received = -1
        var receiveError: Int32 = 0
        var receivedFlags: Int32 = 0
        var controlLength = 0

        while true {
            control = [UInt8](repeating: 0, count: controlSize)
            received = -1
            receiveError = 0
            controlLength = 0
            withUnsafeMutablePointer(to: &marker) { markerPointer in
                var vector = iovec(
                    iov_base: UnsafeMutableRawPointer(markerPointer),
                    iov_len: 1
                )
                withUnsafeMutablePointer(to: &vector) { vectorPointer in
                    control.withUnsafeMutableBytes { controlBytes in
                        var message = msghdr()
                        message.msg_iov = vectorPointer
                        message.msg_iovlen = 1
                        message.msg_control = controlBytes.baseAddress
                        message.msg_controllen = socklen_t(controlBytes.count)
                        let result = Darwin.recvmsg(socket, &message, 0)
                        received = result
                        if result < 0 {
                            receiveError = errno
                        }
                        receivedFlags = message.msg_flags
                        controlLength = Int(message.msg_controllen)
                    }
                }
            }
            if received >= 0 || receiveError != EINTR {
                break
            }
        }

        if received < 0 {
            throw POSIXError(POSIXErrorCode(rawValue: receiveError) ?? .EIO)
        }
        let usedControl = min(controlLength, control.count)
        var descriptors: [Int32] = []
        var parseError = false
        control.withUnsafeBytes { controlBytes in
            guard let baseAddress = controlBytes.baseAddress else {
                parseError = true
                return
            }
            var offset = 0
            while offset + headerSize <= usedControl {
                let header = baseAddress
                    .advanced(by: offset)
                    .assumingMemoryBound(to: cmsghdr.self)
                    .pointee
                let messageLength = Int(header.cmsg_len)
                guard messageLength >= dataOffset,
                      messageLength <= usedControl - offset else {
                    parseError = true
                    return
                }
                if header.cmsg_level == SOL_SOCKET && header.cmsg_type == SCM_RIGHTS {
                    let byteCount = messageLength - dataOffset
                    guard byteCount >= MemoryLayout<Int32>.size,
                          byteCount % MemoryLayout<Int32>.size == 0 else {
                        parseError = true
                        return
                    }
                    let dataAddress = baseAddress.advanced(by: offset + dataOffset)
                    for index in 0..<(byteCount / MemoryLayout<Int32>.size) {
                        let descriptor = dataAddress
                            .advanced(by: index * MemoryLayout<Int32>.size)
                            .loadUnaligned(as: Int32.self)
                        descriptors.append(descriptor)
                    }
                }
                let nextOffset = offset + align(messageLength)
                guard nextOffset > offset else {
                    parseError = true
                    return
                }
                if nextOffset >= usedControl {
                    offset = usedControl
                    break
                }
                offset = nextOffset
            }
            if usedControl - offset >= headerSize {
                parseError = true
            }
        }
        guard received == 1,
              marker == expectedMarker,
              receivedFlags & (MSG_CTRUNC | MSG_TRUNC) == 0,
              !parseError,
              descriptors.count == 1,
              descriptors[0] >= 0 else {
            for descriptor in descriptors where descriptor >= 0 {
                Darwin.close(descriptor)
            }
            throw POSIXError(.EPROTO)
        }
        let descriptor = descriptors[0]
        if Darwin.fcntl(descriptor, F_SETFD, FD_CLOEXEC) < 0 {
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        return descriptor
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
        let entry = try WireEntry(reader: &reader, includesContentGeneration: control.supportsContentGeneration)
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
            entries.append(try WireEntry(reader: &reader, includesContentGeneration: control.supportsContentGeneration))
        }
        try reader.finish()
        return entries
    }

    func open(_ path: String, flags: Int32, connection: WireConnection) throws -> WireOpenResult {
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(UInt32(bitPattern: flags))
        let response = try connection.requestWithReadFD(.open, payload: writer.data)
        var nativeReadFD = response.nativeReadFD
        let sharedReadFD = response.sharedReadFD
        var sharedWindowFD = response.sharedWindowFD
        do {
            var reader = WireReader(response.payload)
            let handle = try reader.uint64()
            var sharedReadWindow: WireSharedReadWindow?
            switch response.flags {
            case 0:
                guard nativeReadFD == nil,
                      sharedReadFD == nil,
                      sharedWindowFD == nil else {
                    throw POSIXError(.EPROTO)
                }
            case flagNativeReadFD:
                guard nativeReadFD != nil,
                      sharedReadFD == nil,
                      sharedWindowFD == nil else {
                    throw POSIXError(.EPROTO)
                }
            case flagSharedWindow, flagSharedFileWindow:
                guard nativeReadFD == nil,
                      sharedReadFD == nil,
                      let descriptor = sharedWindowFD else {
                    throw POSIXError(.EPROTO)
                }
                let capacity = Int(try reader.uint32())
                sharedWindowFD = nil
                if response.flags == flagSharedFileWindow {
                    sharedReadWindow = try WireSharedReadWindow(
                        fileDescriptor: descriptor,
                        capacity: capacity
                    )
                } else {
                    sharedReadWindow = try WireSharedReadWindow(
                        descriptor: descriptor,
                        capacity: capacity
                    )
                }
            default:
                throw POSIXError(.EPROTO)
            }
            try reader.finish()
            let result = WireOpenResult(
                handle: handle,
                nativeReadFD: nativeReadFD,
                sharedReadWindow: sharedReadWindow
            )
            nativeReadFD = nil
            return result
        } catch {
            if let nativeReadFD {
                Darwin.close(nativeReadFD)
            }
            if let sharedReadFD {
                Darwin.close(sharedReadFD)
            }
            if let sharedWindowFD {
                Darwin.close(sharedWindowFD)
            }
            throw error
        }
    }

    func create(_ path: String, flags: Int32) throws -> (WireConnection, UInt64, WireEntry) {
        let connection = try newConnection()
        var writer = WireWriter()
        writer.string(path)
        writer.uint32(UInt32(bitPattern: flags))
        var reader = WireReader(try connection.request(.create, payload: writer.data))
        let handle = try reader.uint64()
        let entry = try WireEntry(reader: &reader, includesContentGeneration: connection.supportsContentGeneration)
        try reader.finish()
        return (connection, handle, entry)
    }

    func read(
        handle: UInt64,
        offset: Int64,
        length: Int,
        connection: WireConnection,
        sharedWindow: WireSharedReadWindow? = nil
    ) throws -> Data {
        guard offset >= 0, length >= 0 else { throw POSIXError(.EINVAL) }
        guard length > 0 else { return Data() }
        let maximumReadBytes = connection.maximumReadBytes
        guard maximumReadBytes > 0 else { throw POSIXError(.EPROTO) }
        if length <= maximumReadBytes {
            return try readChunk(
                handle: handle,
                offset: offset,
                length: length,
                connection: connection,
                sharedWindow: sharedWindow
            )
        }
        var result = Data(capacity: length)
        var completed = 0
        while completed < length {
            let chunkLength = min(maximumReadBytes, length - completed)
            let chunk = try readChunk(
                handle: handle,
                offset: offset + Int64(completed),
                length: chunkLength,
                connection: connection,
                sharedWindow: sharedWindow
            )
            result.append(chunk)
            completed += chunk.count
            if chunk.count < chunkLength {
                break
            }
        }
        return result
    }

    func readBorrowingSharedWindow(
        handle: UInt64,
        offset: Int64,
        length: Int,
        connection: WireConnection,
        sharedWindow: WireSharedReadWindow
    ) throws -> WireReadResult {
        guard offset >= 0, length > 0, length <= connection.maximumReadBytes else {
            throw POSIXError(.EINVAL)
        }
        var writer = WireWriter()
        writer.uint64(handle)
        writer.int64(offset)
        writer.uint32(UInt32(length))
        return try connection.requestReadBorrowingSharedWindow(
            payload: writer.data,
            requestedLength: length,
            sharedWindow: sharedWindow
        )
    }

    private func readChunk(
        handle: UInt64,
        offset: Int64,
        length: Int,
        connection: WireConnection,
        sharedWindow: WireSharedReadWindow?
    ) throws -> Data {
        var writer = WireWriter()
        writer.uint64(handle)
        writer.int64(offset)
        writer.uint32(UInt32(length))
        return try connection.requestRead(
            payload: writer.data,
            requestedLength: length,
            sharedWindow: sharedWindow
        )
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
