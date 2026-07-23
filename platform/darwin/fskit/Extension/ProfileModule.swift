import Darwin
import Dispatch
import ExtensionFoundation
import Foundation
import FSKit
import OSLog

@main
struct CodexFoldFSKitModule: UnaryFileSystemExtension {
    var fileSystem: FSUnaryFileSystem & FSUnaryFileSystemOperations {
        CodexFoldFileSystem()
    }
}

final class CodexFoldFileSystem: FSUnaryFileSystem, FSUnaryFileSystemOperations {
    private let logger = Logger(
        subsystem: "vip.jstar.codexfold.fskitprofileprobe.module",
        category: "resource"
    )
    private let volumeID = FSVolume.Identifier(uuid: UUID(uuidString: "5D0CF927-75A7-48B0-BDAE-621D8F2E695B")!)
    private let lock = NSLock()
    private var activeResourceURL: URL?
    private weak var activeVolume: CodexFoldVolume?

    func probeResource(
        resource: FSResource,
        replyHandler: @escaping (FSProbeResult?, (any Error)?) -> Void
    ) {
        let containerID = FSContainerIdentifier(uuid: volumeID.uuid)
        replyHandler(.usable(name: "CodexFold", containerID: containerID), nil)
    }

    func loadResource(
        resource: FSResource,
        options: FSTaskOptions,
        replyHandler: @escaping (FSVolume?, (any Error)?) -> Void
    ) {
        guard let pathResource = resource as? FSPathURLResource else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        let url = pathResource.url
        let scoped = url.startAccessingSecurityScopedResource()
        logger.notice("loadResource started scoped=\(scoped, privacy: .public)")
        do {
            var isDirectory = ObjCBool(false)
            guard FileManager.default.fileExists(atPath: url.path, isDirectory: &isDirectory) else {
                logger.error("resource path is missing")
                throw POSIXError(.ENOENT)
            }
            logger.notice("resource inspected directory=\(isDirectory.boolValue, privacy: .public)")
            let descriptorURL = isDirectory.boolValue
                ? url.appendingPathComponent("descriptor.bin", isDirectory: false)
                : url
            let descriptorData = try Data(contentsOf: descriptorURL)
            logger.notice("descriptor read bytes=\(descriptorData.count, privacy: .public)")
            let descriptor = try WireDescriptor(data: descriptorData)
            logger.notice("descriptor decoded generation=\(descriptor.generation, privacy: .public)")
            logger.notice("connecting to daemon socket")
            let client = try DaemonClient(descriptor: descriptor)
            logger.notice("daemon socket connected; sending ping")
            try client.ping()
            logger.notice("daemon ping succeeded")
            let volume = try CodexFoldVolume(volumeID: volumeID, client: client)
            lock.lock()
            activeResourceURL = scoped ? url : nil
            activeVolume = volume
            lock.unlock()
            containerStatus = .ready
            replyHandler(volume, nil)
        } catch {
            logger.error("loadResource failed: \(String(describing: error), privacy: .public)")
            if scoped {
                url.stopAccessingSecurityScopedResource()
            }
            replyHandler(nil, error)
        }
    }

    func unloadResource(resource: FSResource, options: FSTaskOptions) async throws {
		let (volume, url) = lock.withLock { () -> (CodexFoldVolume?, URL?) in
			let volume = activeVolume
			let url = activeResourceURL
			activeVolume = nil
			activeResourceURL = nil
			return (volume, url)
		}
        try volume?.synchronizeNow()
        url?.stopAccessingSecurityScopedResource()
        containerStatus = .notReady(status: POSIXError(.EAGAIN))
    }
}

private final class CodexFoldPrefetchChannel {
    let connection: WireConnection
    let handle: UInt64
    let generation: UInt64
    let sharedReadWindow: WireSharedReadWindow?
    private(set) var nativeReadFD: Int32?
    private var closed = false

    init(connection: WireConnection, opened: WireOpenResult, generation: UInt64) {
        self.connection = connection
        self.handle = opened.handle
        self.generation = generation
        self.nativeReadFD = opened.nativeReadFD
        self.sharedReadWindow = opened.sharedReadWindow
    }

    func close(client: DaemonClient) {
        guard !closed else { return }
        closed = true
        if let nativeReadFD {
            Darwin.close(nativeReadFD)
            self.nativeReadFD = nil
        }
        try? client.handleOperation(.release, handle: handle, connection: connection)
        connection.close()
    }

    deinit {
        if let nativeReadFD {
            Darwin.close(nativeReadFD)
        }
        connection.close()
    }
}

private final class CodexFoldIO {
    // Keep the cache bounded while amortizing the FSKit-to-daemon round trip.
    // A 12 MiB block aligns with the mounted reader's 4 MiB requests and lets
    // eight workers maintain a 96 MiB horizon when the kernel cache is off.

    let client: DaemonClient
    let connection: WireConnection
    let handle: UInt64
    let path: String
    let writable: Bool
    private let foregroundFetchLock = NSLock()
    private let cacheLock = NSLock()
    private let nativeFDLock = NSLock()
    private let prefetchPoolLock = NSLock()
    private let prefetchQueue: OperationQueue
    private var cachedBlocks: [Int64: CodexFoldCachedReadBlock] = [:]
    private var prefetchingBlocks: Set<Int64> = []
    private var prefetchGroups: [Int64: DispatchGroup] = [:]
    private var prefetchFrontier: Int64 = 0
    private var sequentialHintBlockOffset: Int64?
    private var cachePruneOffset: Int64 = 0
    private var cacheGeneration: UInt64 = 1
    private var fileSize: Int64
    private var closed = false
    private var nativeReadFD: Int32?
    private let sharedReadWindow: WireSharedReadWindow?
    private var idlePrefetchChannels: [CodexFoldPrefetchChannel] = []
    private var prefetchChannelCount = 0
    private var prefetchPoolGeneration: UInt64 = 1
    private var prefetchPoolClosed = false
    private let readAheadBytes: Int
    private let concurrentPrefetchCount: Int
    private let scheduledPrefetchCount: Int
    private let maxCachedBlocks: Int

    init(
        client: DaemonClient,
        connection: WireConnection,
        handle: UInt64,
        path: String,
        writable: Bool,
        size: UInt64,
        nativeReadFD: Int32? = nil,
        sharedReadWindow: WireSharedReadWindow? = nil
    ) {
        self.client = client
        self.connection = connection
        self.handle = handle
        self.path = path
        self.writable = writable
        self.fileSize = Self.normalizedFileSize(size)
        let readAheadPolicy = CodexFoldReadAheadPolicy(
            negotiatedReadBytes: connection.maximumReadBytes
        )
        self.readAheadBytes = readAheadPolicy.readAheadBytes
        self.concurrentPrefetchCount = readAheadPolicy.concurrentPrefetchCount
        self.scheduledPrefetchCount = readAheadPolicy.scheduledPrefetchCount
        self.maxCachedBlocks = readAheadPolicy.maxCachedBlocks
        let prefetchQueue = OperationQueue()
        prefetchQueue.name = "vip.jstar.codexfold.fskit.readahead.\(handle)"
        prefetchQueue.qualityOfService = .userInitiated
        prefetchQueue.maxConcurrentOperationCount = concurrentPrefetchCount
        self.prefetchQueue = prefetchQueue
        if writable {
            if let nativeReadFD {
                Darwin.close(nativeReadFD)
            }
            self.nativeReadFD = nil
            self.sharedReadWindow = nil
        } else {
            self.nativeReadFD = nativeReadFD
            self.sharedReadWindow = sharedReadWindow
        }
        primePrefetchChannelsIfBeneficial()
        primeReadAheadIfBeneficial()
    }

    func read(
        client: DaemonClient,
        offset: Int64,
        length: Int,
        into buffer: FSMutableFileDataBuffer
    ) throws -> Int {
        guard offset >= 0, length >= 0, buffer.length >= length else {
            throw POSIXError(.EINVAL)
        }
        guard length > 0 else { return 0 }
        if !writable {
            nativeFDLock.lock()
            if let nativeReadFD {
                defer { nativeFDLock.unlock() }
                return try Self.readNativeDescriptor(
                    nativeReadFD,
                    offset: offset,
                    length: length,
                    into: buffer
                )
            }
            nativeFDLock.unlock()
        }
        if !writable, length < readAheadBytes {
            let blockSize = Int64(readAheadBytes)
            let fetchOffset = offset / blockSize * blockSize
            if let copied = copyCachedRange(offset: offset, length: length, into: buffer) {
                schedulePrefetch(after: fetchOffset, generation: currentCacheGeneration())
                return copied
            }

            foregroundFetchLock.lock()
            defer { foregroundFetchLock.unlock() }
            if let copied = copyCachedRange(offset: offset, length: length, into: buffer) {
                schedulePrefetch(after: fetchOffset, generation: currentCacheGeneration())
                return copied
            }
            waitForPrefetch(offset: fetchOffset)
            if let copied = copyCachedRange(offset: offset, length: length, into: buffer) {
                schedulePrefetch(after: fetchOffset, generation: currentCacheGeneration())
                return copied
            }
            guard !isClosed() else { throw POSIXError(.EBADF) }

            if readAheadLength(offset: fetchOffset) > 0 {
                let generation = currentCacheGeneration()
                schedulePrefetchBlock(offset: fetchOffset, generation: generation)
                waitForPrefetch(offset: fetchOffset)
                if let copied = copyCachedRange(offset: offset, length: length, into: buffer) {
                    schedulePrefetch(after: fetchOffset, generation: generation)
                    return copied
                }
            }
        }
        let data = try client.read(
            handle: handle,
            offset: offset,
            length: length,
            connection: connection,
            sharedWindow: sharedReadWindow
        )
        return buffer.withUnsafeMutableBytes { destination in
            _ = data.copyBytes(to: destination.bindMemory(to: UInt8.self))
            return data.count
        }
    }

    func invalidateReadCache() {
        cacheLock.lock()
        resetReadCacheLocked()
        cacheLock.unlock()
        invalidatePrefetchChannels()
    }

    var usesNativeReadFD: Bool {
        nativeFDLock.lock()
        defer { nativeFDLock.unlock() }
        return nativeReadFD != nil
    }

    func updateFileSize(_ size: UInt64) {
        let updated = Self.normalizedFileSize(size)
        var changed = false
        cacheLock.lock()
        if updated != fileSize {
            fileSize = updated
            resetReadCacheLocked()
            changed = true
        }
        cacheLock.unlock()
        if changed {
            invalidatePrefetchChannels()
        }
    }

    func shutdown() {
        cacheLock.lock()
        closed = true
        resetReadCacheLocked()
        cacheLock.unlock()
        prefetchQueue.cancelAllOperations()
        prefetchQueue.waitUntilAllOperationsAreFinished()

        nativeFDLock.lock()
        if let nativeReadFD {
            Darwin.close(nativeReadFD)
            self.nativeReadFD = nil
        }
        nativeFDLock.unlock()

        closePrefetchPool()
    }

    private func readNativeDescriptorIfAvailable(offset: Int64, length: Int) throws -> Data? {
        nativeFDLock.lock()
        defer { nativeFDLock.unlock() }
        guard let nativeReadFD else { return nil }
        return try Self.readNativeDescriptor(nativeReadFD, offset: offset, length: length)
    }

    private static func normalizedFileSize(_ size: UInt64) -> Int64 {
        size > UInt64(Int64.max) ? Int64.max : Int64(size)
    }

    private func resetReadCacheLocked() {
        cacheGeneration &+= 1
        cachedBlocks.removeAll(keepingCapacity: false)
        prefetchingBlocks.removeAll(keepingCapacity: false)
        prefetchGroups.removeAll(keepingCapacity: false)
        prefetchFrontier = 0
        sequentialHintBlockOffset = nil
        cachePruneOffset = 0
    }

    private func checkoutPrefetchChannel() throws -> CodexFoldPrefetchChannel? {
        prefetchPoolLock.lock()
        if prefetchPoolClosed {
            prefetchPoolLock.unlock()
            return nil
        }
        if let channel = idlePrefetchChannels.popLast() {
            prefetchPoolLock.unlock()
            return channel
        }
        guard prefetchChannelCount < maxCachedBlocks else {
            prefetchPoolLock.unlock()
            return nil
        }
        let generation = prefetchPoolGeneration
        prefetchChannelCount += 1
        prefetchPoolLock.unlock()

        let connection: WireConnection
        do {
            connection = try client.newConnection()
        } catch {
            releasePrefetchChannelReservation()
            throw error
        }
        let channel: CodexFoldPrefetchChannel
        do {
            let opened = try client.open(path, flags: O_RDONLY, connection: connection)
            channel = CodexFoldPrefetchChannel(
                connection: connection,
                opened: opened,
                generation: generation
            )
        } catch {
            connection.close()
            releasePrefetchChannelReservation()
            throw error
        }

        prefetchPoolLock.lock()
        let accepted = !prefetchPoolClosed && generation == prefetchPoolGeneration
        if !accepted {
            prefetchChannelCount -= 1
        }
        prefetchPoolLock.unlock()
        if !accepted {
            channel.close(client: client)
            return nil
        }
        return channel
    }

    private func primePrefetchChannelsIfBeneficial() {
        guard shouldPrimeReadAhead else {
            return
        }

        var channels: [CodexFoldPrefetchChannel] = []
        channels.reserveCapacity(maxCachedBlocks)
        for _ in 0..<maxCachedBlocks {
            do {
                guard let channel = try checkoutPrefetchChannel() else { break }
                channels.append(channel)
            } catch {
                break
            }
        }
        for channel in channels {
            returnPrefetchChannel(channel, healthy: true)
        }
    }

    private func primeReadAheadIfBeneficial() {
        guard shouldPrimeReadAhead else {
            return
        }
        let generation = currentCacheGeneration()
        let blockSize = Int64(readAheadBytes)
        for index in 0..<scheduledPrefetchCount {
            schedulePrefetchBlock(offset: Int64(index) * blockSize, generation: generation)
        }
    }

    private var shouldPrimeReadAhead: Bool {
        !writable &&
            nativeReadFD == nil &&
            sharedReadWindow != nil &&
            fileSize >= Int64(readAheadBytes * maxCachedBlocks)
    }

    private func returnPrefetchChannel(_ channel: CodexFoldPrefetchChannel, healthy: Bool) {
        prefetchPoolLock.lock()
        let retain = healthy &&
            !prefetchPoolClosed &&
            channel.generation == prefetchPoolGeneration
        if retain {
            idlePrefetchChannels.append(channel)
        } else {
            prefetchChannelCount -= 1
        }
        prefetchPoolLock.unlock()
        if !retain {
            channel.close(client: client)
        }
    }

    private func releasePrefetchChannelReservation() {
        prefetchPoolLock.lock()
        prefetchChannelCount -= 1
        prefetchPoolLock.unlock()
    }

    private func invalidatePrefetchChannels() {
        prefetchPoolLock.lock()
        prefetchPoolGeneration &+= 1
        let channels = idlePrefetchChannels
        idlePrefetchChannels.removeAll(keepingCapacity: false)
        prefetchChannelCount -= channels.count
        prefetchPoolLock.unlock()
        for channel in channels {
            channel.close(client: client)
        }
    }

    private func closePrefetchPool() {
        prefetchPoolLock.lock()
        prefetchPoolClosed = true
        prefetchPoolGeneration &+= 1
        let channels = idlePrefetchChannels
        idlePrefetchChannels.removeAll(keepingCapacity: false)
        prefetchChannelCount -= channels.count
        prefetchPoolLock.unlock()
        for channel in channels {
            channel.close(client: client)
        }
    }

    private func readAheadLength(offset: Int64) -> Int {
        cacheLock.lock()
        defer { cacheLock.unlock() }
        return readAheadLengthLocked(offset: offset)
    }

    private func readAheadLengthLocked(offset: Int64) -> Int {
        guard offset >= 0, offset < fileSize else { return 0 }
        return Int(min(Int64(readAheadBytes), fileSize - offset))
    }

    private static func readNativeDescriptor(_ descriptor: Int32, offset: Int64, length: Int) throws -> Data {
        guard descriptor >= 0, offset >= 0, length >= 0 else {
            throw POSIXError(.EINVAL)
        }
        var result = Data(count: length)
        var completed = 0
        while completed < length {
            let amount = result.withUnsafeMutableBytes { bytes in
                Darwin.pread(
                    descriptor,
                    bytes.baseAddress!.advanced(by: completed),
                    length - completed,
                    off_t(offset + Int64(completed))
                )
            }
            if amount == 0 {
                break
            }
            if amount < 0 {
                if errno == EINTR { continue }
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
            completed += amount
        }
        if completed < result.count {
            result.removeSubrange(completed..<result.count)
        }
        return result
    }

    private static func readNativeDescriptor(
        _ descriptor: Int32,
        offset: Int64,
        length: Int,
        into buffer: FSMutableFileDataBuffer
    ) throws -> Int {
        guard descriptor >= 0, offset >= 0, length >= 0, buffer.length >= length else {
            throw POSIXError(.EINVAL)
        }
        return try buffer.withUnsafeMutableBytes { destination in
            var completed = 0
            while completed < length {
                let amount = Darwin.pread(
                    descriptor,
                    destination.baseAddress!.advanced(by: completed),
                    length - completed,
                    off_t(offset + Int64(completed))
                )
                if amount == 0 {
                    break
                }
                if amount < 0 {
                    if errno == EINTR { continue }
                    throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
                }
                completed += amount
            }
            return completed
        }
    }

    private func copyCachedRange(
        offset: Int64,
        length: Int,
        into buffer: FSMutableFileDataBuffer
    ) -> Int? {
        cacheLock.lock()
        guard !closed, offset >= 0, length >= 0, offset < fileSize else {
            cacheLock.unlock()
            return nil
        }
        let requestedEnd = Int64(length) > Int64.max - offset ? Int64.max : offset + Int64(length)
        let end = min(fileSize, requestedEnd)
        guard end >= offset, end - offset <= Int64(buffer.length) else {
            cacheLock.unlock()
            return nil
        }
        let blockSize = Int64(readAheadBytes)
        let firstBlockOffset = offset / blockSize * blockSize
        if let cached = cachedBlocks[firstBlockOffset] {
            let lower = offset - firstBlockOffset
            let available = Int64(cached.count) - lower
            if lower >= 0, available >= end - offset {
                pruneCacheLocked(before: firstBlockOffset)
                cacheLock.unlock()
                return copyCachedSpan(
                    cached,
                    sourceOffset: Int(lower),
                    length: Int(end - offset),
                    into: buffer
                )
            }
        }

        var spans: [(block: CodexFoldCachedReadBlock, offset: Int, length: Int)] = []
        var current = offset
        while current < end {
            let blockOffset = current / blockSize * blockSize
            guard let cached = cachedBlocks[blockOffset] else {
                cacheLock.unlock()
                return nil
            }
            let lower = current - blockOffset
            guard lower >= 0, lower < Int64(cached.count) else {
                cacheLock.unlock()
                return nil
            }
            let available = min(Int64(cached.count) - lower, end - current)
            guard available > 0 else {
                cacheLock.unlock()
                return nil
            }
            spans.append((cached, Int(lower), Int(available)))
            current += available
        }
        pruneCacheLocked(before: firstBlockOffset)
        cacheLock.unlock()

        let written = buffer.withUnsafeMutableBytes { destination in
            var written = 0
            for span in spans {
                guard span.block.copyBytes(
                    from: span.offset,
                    count: span.length,
                    to: destination.baseAddress!.advanced(by: written)
                ) else {
                    return -1
                }
                written += span.length
            }
            return written
        }
        return written >= 0 ? written : nil
    }

    private func copyCachedSpan(
        _ block: CodexFoldCachedReadBlock,
        sourceOffset: Int,
        length: Int,
        into buffer: FSMutableFileDataBuffer
    ) -> Int? {
        let copied = buffer.withUnsafeMutableBytes { destination in
            block.copyBytes(
                from: sourceOffset,
                count: length,
                to: destination.baseAddress!
            )
        }
        return copied ? length : nil
    }

    private func pruneCacheLocked(before offset: Int64) {
        guard offset > cachePruneOffset else { return }
        let staleOffsets = cachedBlocks.keys.filter { $0 < offset }
        for staleOffset in staleOffsets {
            cachedBlocks.removeValue(forKey: staleOffset)
        }
        cachePruneOffset = offset
    }

    private func currentCacheGeneration() -> UInt64 {
        cacheLock.lock()
        defer { cacheLock.unlock() }
        return cacheGeneration
    }

    private func isClosed() -> Bool {
        cacheLock.lock()
        defer { cacheLock.unlock() }
        return closed
    }

    private func waitForPrefetch(offset: Int64) {
        cacheLock.lock()
        let group = prefetchGroups[offset]
        cacheLock.unlock()
        guard let group else { return }
        // Failed and cancelled prefetches also leave the group, allowing the
        // foreground request to retry through the ordinary path.
        _ = group.wait(timeout: .now() + .seconds(5))
    }

    private func trimCacheLocked() {
        while cachedBlocks.count > maxCachedBlocks {
            guard let oldest = cachedBlocks.keys.min() else { return }
            cachedBlocks.removeValue(forKey: oldest)
        }
    }

    private func schedulePrefetchBlock(offset: Int64, generation: UInt64) {
        var request: (offset: Int64, group: DispatchGroup)?
        cacheLock.lock()
        if !closed,
           generation == cacheGeneration,
           readAheadLengthLocked(offset: offset) > 0,
           cachedBlocks[offset] == nil,
           !prefetchingBlocks.contains(offset),
           prefetchingBlocks.count < scheduledPrefetchCount {
            let group = DispatchGroup()
            group.enter()
            prefetchingBlocks.insert(offset)
            prefetchGroups[offset] = group
            request = (offset, group)
        }
        cacheLock.unlock()
        guard let request else { return }
        prefetchQueue.addOperation { [weak self] in
            guard let self else {
                request.group.leave()
                return
            }
            self.prefetch(offset: request.offset, generation: generation, completion: request.group)
        }
    }

    private func schedulePrefetch(after blockOffset: Int64, generation: UInt64) {
        guard !writable else { return }
        let blockSize = Int64(readAheadBytes)
        var requests: [(offset: Int64, group: DispatchGroup)] = []
        cacheLock.lock()
        guard !closed, generation == cacheGeneration else {
            cacheLock.unlock()
            return
        }
        guard fileSize > 0 else {
            cacheLock.unlock()
            return
        }
        let lastBlockOffset = (fileSize - 1) / blockSize * blockSize
        if blockOffset >= lastBlockOffset {
            cacheLock.unlock()
            return
        }
        if let previous = sequentialHintBlockOffset, blockOffset < previous {
            // A backwards seek starts a new read-ahead horizon without invalidating
            // already fetched blocks that may still satisfy another open reader.
            prefetchFrontier = blockOffset + blockSize
        }
        sequentialHintBlockOffset = blockOffset
        let span = Int64(scheduledPrefetchCount) * blockSize
        let requestedHorizon = blockOffset > Int64.max - span ? Int64.max : blockOffset + span
        let horizon = min(lastBlockOffset, requestedHorizon)
        if prefetchFrontier <= blockOffset {
            prefetchFrontier = blockOffset + blockSize
        }
        while prefetchingBlocks.count < scheduledPrefetchCount && prefetchFrontier <= horizon && prefetchFrontier < fileSize {
            let offset = prefetchFrontier
            prefetchFrontier = offset >= lastBlockOffset ? fileSize : offset + blockSize
            if cachedBlocks[offset] != nil || prefetchingBlocks.contains(offset) {
                continue
            }
            let group = DispatchGroup()
            group.enter()
            prefetchingBlocks.insert(offset)
            prefetchGroups[offset] = group
            requests.append((offset, group))
        }
        cacheLock.unlock()

        for request in requests {
            prefetchQueue.addOperation { [weak self] in
                guard let self else {
                    request.group.leave()
                    return
                }
                self.prefetch(offset: request.offset, generation: generation, completion: request.group)
            }
        }
    }

    private func prefetch(offset: Int64, generation: UInt64, completion: DispatchGroup) {
        var nextHint: Int64?
        defer {
            cacheLock.lock()
            if prefetchGroups[offset] === completion {
                prefetchingBlocks.remove(offset)
                prefetchGroups.removeValue(forKey: offset)
            }
            if !closed && generation == cacheGeneration {
                nextHint = sequentialHintBlockOffset
            }
            cacheLock.unlock()
            completion.leave()
            if let nextHint {
                schedulePrefetch(after: nextHint, generation: generation)
            }
        }
        cacheLock.lock()
        let allowed = !closed && generation == cacheGeneration
        let fetchLength = allowed ? readAheadLengthLocked(offset: offset) : 0
        cacheLock.unlock()
        guard allowed, fetchLength > 0 else { return }

        do {
            if let data = try readNativeDescriptorIfAvailable(offset: offset, length: fetchLength) {
                cacheLock.lock()
                if !closed && generation == cacheGeneration {
                    cachedBlocks[offset] = CodexFoldCachedReadBlock(data: data)
                    trimCacheLocked()
                }
                cacheLock.unlock()
                return
            }
        } catch {
            // The ordinary daemon path below remains the compatibility fallback.
        }

        var channel: CodexFoldPrefetchChannel?
        var healthy = false
        defer {
            if let channel {
                returnPrefetchChannel(channel, healthy: healthy)
            }
        }
        do {
            guard let checkedOut = try checkoutPrefetchChannel() else { return }
            channel = checkedOut
            let block: CodexFoldCachedReadBlock
            if let nativeReadFD = checkedOut.nativeReadFD {
                block = CodexFoldCachedReadBlock(
                    data: try Self.readNativeDescriptor(nativeReadFD, offset: offset, length: fetchLength)
                )
            } else if let sharedWindow = checkedOut.sharedReadWindow {
                let result = try client.readBorrowingSharedWindow(
                    handle: checkedOut.handle,
                    offset: offset,
                    length: fetchLength,
                    connection: checkedOut.connection,
                    sharedWindow: sharedWindow
                )
                switch result {
                case .copied(let data):
                    block = CodexFoldCachedReadBlock(data: data)
                case .sharedWindow(let count):
                    block = try CodexFoldCachedReadBlock(
                        sharedWindow: sharedWindow,
                        count: count,
                        release: { [weak self, checkedOut] in
                            self?.returnPrefetchChannel(checkedOut, healthy: true)
                        }
                    )
                    channel = nil
                }
            } else {
                block = CodexFoldCachedReadBlock(
                    data: try client.read(
                        handle: checkedOut.handle,
                        offset: offset,
                        length: fetchLength,
                        connection: checkedOut.connection
                    )
                )
            }
            healthy = true
            cacheLock.lock()
            if !closed && generation == cacheGeneration {
                cachedBlocks[offset] = block
                trimCacheLocked()
            }
            cacheLock.unlock()
        } catch {
            return
        }
    }
}

private final class CodexFoldItem: FSItem {
    private let lock = NSLock()
    private var storedEntry: WireEntry
    private var storedIO: CodexFoldIO?
    private(set) var deleted = false

    init(entry: WireEntry) {
        storedEntry = entry
        super.init()
    }

    var entry: WireEntry {
        lock.lock()
        defer { lock.unlock() }
        return storedEntry
    }

    func update(_ entry: WireEntry) {
        lock.lock()
        storedEntry = entry
        deleted = false
        let currentIO = storedIO
        lock.unlock()
        currentIO?.updateFileSize(entry.size)
    }

    func markDeleted() {
        lock.lock()
        deleted = true
        lock.unlock()
    }

    func io() -> CodexFoldIO? {
        lock.lock()
        defer { lock.unlock() }
        return storedIO
    }

    func replaceIO(_ io: CodexFoldIO?) -> CodexFoldIO? {
        lock.lock()
        let previous = storedIO
        storedIO = io
        lock.unlock()
        return previous
    }

    func invalidateReadCache() {
        lock.lock()
        let current = storedIO
        lock.unlock()
        current?.invalidateReadCache()
    }
}

private final class CodexFoldVolume: FSVolume, FSVolume.Handler, FSVolume.ReadWriteHandler, FSVolume.DataCacheHandler, FSVolume.XattrHandler {
    private let logger = Logger(
        subsystem: "vip.jstar.codexfold.fskitprofileprobe.module",
        category: "coherency"
    )
    private let client: DaemonClient
    private let itemLock = NSLock()
    private var items: [UInt64: CodexFoldItem] = [:]
    private var rootItem: CodexFoldItem
    private var namespaceVersion: UInt64
    private var namespaceTimer: DispatchSourceTimer?

    init(volumeID: FSVolume.Identifier, client: DaemonClient) throws {
        self.client = client
        let rootEntry = try client.getattr("/")
        rootItem = CodexFoldItem(entry: rootEntry)
        namespaceVersion = rootEntry.namespaceID
        items[rootEntry.nodeID] = rootItem
        super.init(volumeID: volumeID, volumeName: FSFileName(string: "CodexFold"))
    }

    var supportedVolumeCapabilities: FSVolume.SupportedCapabilities {
        let capabilities = FSVolume.SupportedCapabilities()
        capabilities.supportsPersistentObjectIDs = false
        capabilities.supportsSymbolicLinks = false
        capabilities.supportsHardLinks = false
        capabilities.supportsJournal = false
        capabilities.supportsActiveJournal = false
        capabilities.supportsSparseFiles = false
        // A statfs round trip crosses the extension/daemon boundary, so let
        // FSKit cache it instead of treating it as a local constant-time call.
        capabilities.supportsFastStatFS = false
        capabilities.supports2TBFiles = true
        capabilities.supports64BitObjectIDs = true
        capabilities.supportsHiddenFiles = true
        capabilities.caseFormat = .sensitive
        return capabilities
    }

    var volumeStatistics: FSStatFSResult {
        let result = FSStatFSResult(fileSystemTypeName: "codexfold")
        do {
            let stat = try client.statfs()
            result.blockSize = Int(stat.blockSize)
            result.ioSize = Int(stat.ioSize)
            result.totalBytes = stat.totalBytes
            result.availableBytes = stat.availableBytes
            result.freeBytes = stat.freeBytes
            result.usedBytes = stat.usedBytes
            result.totalFiles = stat.totalFiles
            result.freeFiles = stat.freeFiles
        } catch {
            result.blockSize = 4096
            result.ioSize = 4 * 1024 * 1024
            result.totalBytes = 1 << 40
            result.availableBytes = 1 << 39
            result.freeBytes = 1 << 39
            result.usedBytes = 1 << 39
            result.totalFiles = 1 << 32
            result.freeFiles = 1 << 31
        }
        return result
    }

    var maximumLinkCount: Int { 1 }
    var maximumNameLength: Int { 255 }
    var maximumFileSize: UInt64 { UInt64.max >> 1 }
    var maximumXattrSize: Int { 16 * 1024 * 1024 - 8192 }
    var restrictsOwnershipChanges: Bool { false }
    var truncatesLongNames: Bool { false }
    var enableOpenUnlinkEmulation: Bool { true }

    func activate(
        options: FSTaskOptions,
        replyHandler: @escaping (FSActivateResult?, (any Error)?) -> Void
    ) {
        do {
            let entry = try client.getattr("/")
            rootItem.update(entry)
            itemLock.lock()
            items = [entry.nodeID: rootItem]
            namespaceVersion = entry.namespaceID
            itemLock.unlock()
            startNamespaceMonitor()
            replyHandler(FSActivateResult(rootItem: rootItem), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func deactivate(options: FSDeactivateOptions, replyHandler: @escaping ((any Error)?) -> Void) {
        stopNamespaceMonitor()
        closeAllItems()
        replyHandler(nil)
    }

    func mount(options: FSTaskOptions, replyHandler: @escaping ((any Error)?) -> Void) {
        do {
            try client.ping()
            replyHandler(nil)
        } catch {
            replyHandler(error)
        }
    }

    func unmount(replyHandler: @escaping () -> Void) {
        stopNamespaceMonitor()
        try? synchronizeNow()
        closeAllItems()
        replyHandler()
    }

    func synchronize(flags: FSSyncFlags, replyHandler: @escaping ((any Error)?) -> Void) {
        do {
            try synchronizeNow()
            replyHandler(nil)
        } catch {
            replyHandler(error)
        }
    }

    func synchronizeNow() throws {
        try client.sync()
    }

    func lookupItem(
        named name: FSFileName,
        in directory: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSLookupItemResult?, (any Error)?) -> Void
    ) {
        guard let directory = directory as? CodexFoldItem, directory.entry.type == .directory else {
            replyHandler(nil, POSIXError(.ENOTDIR))
            return
        }
        guard let nameString = name.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let entry = try client.getattr(join(directory.entry.path, nameString))
            let item = item(for: entry)
            replyHandler(
                FSLookupItemResult(foundItem: item, itemName: FSFileName(string: entry.name), itemAttributes: attributes(for: entry)),
                nil
            )
        } catch {
            replyHandler(nil, error)
        }
    }

    func reclaimItem(_ item: FSItem, replyHandler: @escaping ((any Error)?) -> Void) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(POSIXError(.EINVAL))
            return
        }
        itemLock.lock()
        let reclaimed = item.tryReclaim { [self] in
            closeIO(item.replaceIO(nil))
            if items[item.entry.nodeID] === item && item !== rootItem {
                items.removeValue(forKey: item.entry.nodeID)
            }
        }
        itemLock.unlock()
        replyHandler(reclaimed ? nil : nil)
    }

    func createItem(
        named name: FSFileName,
        type: FSItem.ItemType,
        in directory: FSItem,
        attributes newAttributes: FSItem.SetAttributesRequest,
        context: FSContext,
        replyHandler: @escaping (FSCreateItemResult?, (any Error)?) -> Void
    ) {
        guard let directory = directory as? CodexFoldItem, directory.entry.type == .directory else {
            replyHandler(nil, POSIXError(.ENOTDIR))
            return
        }
        guard let nameString = name.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        let itemPath = join(directory.entry.path, nameString)
        do {
            var entry: WireEntry
            switch type {
            case .file:
                let created = try client.create(itemPath, flags: O_RDWR | O_APPEND)
                try client.handleOperation(.release, handle: created.1, connection: created.0)
                created.0.close()
                entry = created.2
            case .directory:
                try client.mkdir(itemPath, mode: newAttributes.isValid(.mode) ? newAttributes.mode : 0o700)
                entry = try client.getattr(itemPath)
            default:
                throw POSIXError(.ENOTSUP)
            }
            try applyAttributes(newAttributes, path: itemPath, type: type)
            entry = try client.getattr(itemPath)
            let item = item(for: entry)
            let directoryEntry = try client.getattr(directory.entry.path)
            directory.update(directoryEntry)
            replyHandler(
                FSCreateItemResult(
                    newItem: item,
                    newItemName: FSFileName(string: entry.name),
                    newItemAttributes: attributes(for: entry),
                    directoryAttributes: attributes(for: directoryEntry),
                    freeSpace: freeSpaceSnapshot()
                ),
                nil
            )
        } catch {
            replyHandler(nil, error)
        }
    }

    func createSymbolicLink(
        named name: FSFileName,
        in directory: FSItem,
        attributes: FSItem.SetAttributesRequest,
        linkContents: FSFileName,
        context: FSContext,
        replyHandler: @escaping (FSCreateSymlinkResult?, (any Error)?) -> Void
    ) {
        replyHandler(nil, POSIXError(.ENOTSUP))
    }

    func createLink(
        to item: FSItem,
        named name: FSFileName,
        in directory: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSCreateLinkResult?, (any Error)?) -> Void
    ) {
        replyHandler(nil, POSIXError(.ENOTSUP))
    }

    func renameItem(
        _ item: FSItem,
        inDirectory sourceDirectory: FSItem,
        named sourceName: FSFileName,
        to destinationName: FSFileName,
        inDirectory destinationDirectory: FSItem,
        overItem: FSItem?,
        context: FSContext,
        replyHandler: @escaping (FSRenameItemResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem,
              let sourceDirectory = sourceDirectory as? CodexFoldItem,
              let destinationDirectory = destinationDirectory as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        guard let sourceNameString = sourceName.string, let destinationNameString = destinationName.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        let sourcePath = join(sourceDirectory.entry.path, sourceNameString)
        let destinationPath = join(destinationDirectory.entry.path, destinationNameString)
        do {
            try client.rename(sourcePath, destinationPath)
            let renamedEntry = try client.getattr(destinationPath)
            item.update(renamedEntry)
            let sourceEntry = try client.getattr(sourceDirectory.entry.path)
            let destinationEntry = sourceDirectory === destinationDirectory ? sourceEntry : try client.getattr(destinationDirectory.entry.path)
            sourceDirectory.update(sourceEntry)
            destinationDirectory.update(destinationEntry)
            if let overItem = overItem as? CodexFoldItem {
                overItem.markDeleted()
                removeCached(overItem)
            }
            replyHandler(
                FSRenameItemResult(
                    newName: FSFileName(string: renamedEntry.name),
                    renamedItemAttributes: attributes(for: renamedEntry),
                    sourceDirectoryAttributes: attributes(for: sourceEntry),
                    destinationDirectoryAttributes: attributes(for: destinationEntry),
                    overItemAttributes: nil,
                    freeSpace: freeSpaceSnapshot()
                ),
                nil
            )
        } catch {
            replyHandler(nil, error)
        }
    }

    func removeItem(
        _ item: FSItem,
        named name: FSFileName,
        from directory: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSRemoveItemResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem, let directory = directory as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        guard let nameString = name.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        let removedEntry = item.entry
        do {
            try client.remove(join(directory.entry.path, nameString), directory: removedEntry.type == .directory)
            item.markDeleted()
            closeIO(item.replaceIO(nil))
            removeCached(item)
            let directoryEntry = try client.getattr(directory.entry.path)
            directory.update(directoryEntry)
            replyHandler(
                FSRemoveItemResult(
                    itemAttributes: attributes(for: removedEntry),
                    directoryAttributes: attributes(for: directoryEntry),
                    freeSpace: freeSpaceSnapshot()
                ),
                nil
            )
        } catch {
            replyHandler(nil, error)
        }
    }

    func getAttributes(
        _ desiredAttributes: FSItem.GetAttributesRequest,
        of item: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSGetAttributesResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let entry = try client.getattr(item.entry.path)
            item.update(entry)
            replyHandler(FSGetAttributesResult(attributes: attributes(for: entry)), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func setAttributes(
        _ newAttributes: FSItem.SetAttributesRequest,
        on item: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSSetAttributesResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            item.invalidateReadCache()
            try applyAttributes(newAttributes, path: item.entry.path, type: itemType(item.entry.type))
            let entry = try client.getattr(item.entry.path)
            item.update(entry)
            replyHandler(FSSetAttributesResult(attributes: attributes(for: entry), freeSpace: freeSpaceSnapshot()), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func enumerateDirectory(
        _ directory: FSItem,
        startingAt cookie: FSDirectoryCookie,
        verifier: FSDirectoryVerifier,
        attributes desiredAttributes: FSItem.GetAttributesRequest?,
        packer: FSDirectoryEntryPacker,
        context: FSContext,
        replyHandler: @escaping (FSEnumerateDirectoryResult?, (any Error)?) -> Void
    ) {
        guard let directory = directory as? CodexFoldItem, directory.entry.type == .directory else {
            replyHandler(nil, POSIXError(.ENOTDIR))
            return
        }
        do {
            let before = try client.getattr(directory.entry.path)
            let entries = try client.readDir(directory.entry.path)
            let after = try client.getattr(directory.entry.path)
            guard before.contentGeneration == after.contentGeneration else {
                throw POSIXError(.ESTALE)
            }
            directory.update(after)
            let currentVersion = after.contentGeneration
            let start = Int(cookie.rawValue)
            guard start >= 0, start <= entries.count else {
                throw POSIXError(.EINVAL)
            }
            if verifier != .initial, verifier.rawValue != currentVersion {
                throw POSIXError(.ESTALE)
            }
            for index in start..<entries.count {
                let entry = entries[index]
                let packed = packer.packEntry(
                    name: FSFileName(string: entry.name),
                    itemType: itemType(entry.type),
                    itemID: FSItem.Identifier(rawValue: entry.nodeID)!,
                    nextCookie: FSDirectoryCookie(rawValue: UInt64(index + 1)),
                    attributes: desiredAttributes == nil ? nil : attributes(for: entry)
                )
                if !packed { break }
            }
            replyHandler(FSEnumerateDirectoryResult(verifier: currentVersion), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func readSymbolicLink(
        _ item: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSReadSymlinkResult?, (any Error)?) -> Void
    ) {
        replyHandler(nil, POSIXError(.ENOTSUP))
    }

    func getXattr(
        named name: FSFileName,
        of item: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSGetXattrResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem, let nameString = name.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let value = try client.getXattr(item.entry.path, name: nameString)
            guard let result = FSGetXattrResult(xattrValue: value) else {
                throw POSIXError(.EIO)
            }
            replyHandler(result, nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func setXattr(
        named name: FSFileName,
        to value: Data?,
        on item: FSItem,
        policy: FSVolume.SetXattrPolicy,
        context: FSContext,
        replyHandler: @escaping (FSSetXattrResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem, let nameString = name.string else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            try client.setXattr(item.entry.path, name: nameString, value: value ?? Data(), policy: UInt32(policy.rawValue))
            if let entry = try? client.getattr(item.entry.path) {
                item.update(entry)
            }
            guard let result = FSSetXattrResult(freeSpace: freeSpaceSnapshot()) else {
                throw POSIXError(.EIO)
            }
            replyHandler(result, nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func listXattrs(
        of item: FSItem,
        context: FSContext,
        replyHandler: @escaping (FSListXattrsResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let names = try client.listXattrs(item.entry.path).map { FSFileName(string: $0) }
            guard let result = FSListXattrsResult(xattrNames: names) else {
                throw POSIXError(.EIO)
            }
            replyHandler(result, nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func read(
        from item: FSItem,
        at offset: off_t,
        length: Int,
        into buffer: FSMutableFileDataBuffer,
        replyHandler: @escaping (FSReadFileResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem, item.entry.type == .file, offset >= 0 else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let io = try ensureIO(item, writable: false)
            let bytesRead = try io.read(client: client, offset: Int64(offset), length: length, into: buffer)
            let entry = item.entry
            replyHandler(FSReadFileResult(bytesRead: bytesRead, itemAttributes: attributes(for: entry)), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func write(
        contents: Data,
        to item: FSItem,
        at offset: off_t,
        replyHandler: @escaping (FSWriteFileResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem, item.entry.type == .file, offset >= 0 else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let io = try ensureIO(item, writable: true)
            let previousSize = item.entry.size
            io.invalidateReadCache()
            let count = try client.write(handle: io.handle, offset: Int64(offset), data: contents, connection: io.connection)
            let entry = try client.getattr(item.entry.path)
            let invalidateKernelCache = writeRequiresKernelCacheInvalidation(
                previousSize: previousSize,
                offset: Int64(offset),
                writtenBytes: count,
                visibleSize: entry.size
            )
            item.update(entry)
            replyHandler(FSWriteFileResult(bytesWritten: count, itemAttributes: attributes(for: entry), freeSpace: freeSpaceSnapshot()), nil)
            if invalidateKernelCache,
               let error = setCacheState(
                   for: item,
                   cacheMode: .none,
                   coherencyType: .noCache,
                   action: .revoke
               ) {
                logger.error("normalized write cache revoke failed: \(String(describing: error), privacy: .public)")
            }
        } catch {
            replyHandler(nil, error)
        }
    }

    func open(
        _ item: FSItem,
        modes: FSVolume.OpenModes,
        cacheMode: FSVolume.DataCacheMode,
        context: FSContext,
        replyHandler: @escaping (FSOpenItemResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let writable = modes.contains(.write)
            var nativePassthrough = false
            if item.entry.type == .file {
                let fileIO = try ensureIO(item, writable: writable)
                nativePassthrough = fileIO.usesNativeReadFD
            }
            let coherency: FSVolume.KernelCacheCoherencyType = !writable && !nativePassthrough && cacheMode != .none ? .readCache : .noCache
            replyHandler(FSOpenItemResult(grantedCoherency: coherency), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    func close(_ item: FSItem, context: FSContext, replyHandler: @escaping () -> Void) {
        if let item = item as? CodexFoldItem {
            let closed = item.replaceIO(nil)
            closeIO(closed)
            if closed?.writable == true {
                if let entry = try? client.getattr(item.entry.path) {
                    item.update(entry)
                }
            }
        }
        replyHandler()
    }

    func upgrade(
        _ item: FSItem,
        cacheMode: FSVolume.DataCacheMode,
        context: FSContext,
        replyHandler: @escaping (FSUpgradeItemResult?, (any Error)?) -> Void
    ) {
        guard let item = item as? CodexFoldItem else {
            replyHandler(nil, POSIXError(.EINVAL))
            return
        }
        do {
            let writable = cacheMode == .readWriteWithCache
            var nativePassthrough = false
            if item.entry.type == .file {
                let fileIO = try ensureIO(item, writable: writable)
                nativePassthrough = fileIO.usesNativeReadFD
            }
            let coherency: FSVolume.KernelCacheCoherencyType = !writable && !nativePassthrough && cacheMode != .none ? .readCache : .noCache
            replyHandler(FSUpgradeItemResult(grantedCoherency: coherency), nil)
        } catch {
            replyHandler(nil, error)
        }
    }

    private func item(for entry: WireEntry) -> CodexFoldItem {
        itemLock.lock()
        defer { itemLock.unlock() }
        if let existing = items[entry.nodeID] {
            existing.update(entry)
            return existing
        }
        let item = CodexFoldItem(entry: entry)
        items[entry.nodeID] = item
        return item
    }

    private func removeCached(_ item: CodexFoldItem) {
        itemLock.lock()
        if items[item.entry.nodeID] === item {
            items.removeValue(forKey: item.entry.nodeID)
        }
        itemLock.unlock()
    }

    private func ensureIO(_ item: CodexFoldItem, writable: Bool) throws -> CodexFoldIO {
        if let existing = item.io(), !writable || existing.writable {
            return existing
        }
        closeIO(item.replaceIO(nil))
        let connection = try client.newConnection()
        var flags: Int32 = writable ? O_RDWR : O_RDONLY
        if writable && item.entry.path.hasSuffix(".jsonl") && !item.entry.name.hasPrefix("._") {
            flags |= O_APPEND
            flags |= Int32(bitPattern: 1 << 31)
        }
        do {
            let opened = try client.open(item.entry.path, flags: flags, connection: connection)
            let io = CodexFoldIO(
                client: client,
                connection: connection,
                handle: opened.handle,
                path: item.entry.path,
                writable: writable,
                size: item.entry.size,
                nativeReadFD: opened.nativeReadFD,
                sharedReadWindow: opened.sharedReadWindow
            )
            closeIO(item.replaceIO(io))
            return io
        } catch {
            connection.close()
            throw error
        }
    }

    private func applyAttributes(
        _ request: FSItem.SetAttributesRequest,
        path: String,
        type: FSItem.ItemType
    ) throws {
        if type == .file, request.isValid(.size) {
            try client.truncate(path, size: request.size)
            request.consumedAttributes.insert(.size)
        }

        let hasMode = request.isValid(.mode)
        let hasUID = request.isValid(.uid)
        let hasGID = request.isValid(.gid)
        let hasAccessTime = request.isValid(.accessTime)
        let hasModifyTime = request.isValid(.modifyTime)
        var valid: UInt32 = 0
        if hasMode { valid |= 1 << 0 }
        if hasUID { valid |= 1 << 1 }
        if hasGID { valid |= 1 << 2 }
        if hasAccessTime { valid |= 1 << 3 }
        if hasModifyTime { valid |= 1 << 4 }
        guard valid != 0 else { return }

        try client.setAttributes(
            path,
            valid: valid,
            mode: hasMode ? request.mode : 0,
            uid: hasUID ? request.uid : 0,
            gid: hasGID ? request.gid : 0,
            accessTime: hasAccessTime ? request.accessTime : timespec(),
            modifyTime: hasModifyTime ? request.modifyTime : timespec()
        )
        if hasMode { request.consumedAttributes.insert(.mode) }
        if hasUID { request.consumedAttributes.insert(.uid) }
        if hasGID { request.consumedAttributes.insert(.gid) }
        if hasAccessTime { request.consumedAttributes.insert(.accessTime) }
        if hasModifyTime { request.consumedAttributes.insert(.modifyTime) }
    }

    private func closeIO(_ io: CodexFoldIO?) {
        guard let io else { return }
        io.shutdown()
        if io.writable {
            try? client.handleOperation(.fsync, handle: io.handle, connection: io.connection)
        }
        try? client.handleOperation(.release, handle: io.handle, connection: io.connection)
        io.connection.close()
    }

    private func closeAllItems() {
        itemLock.lock()
        let snapshot = Array(items.values)
        itemLock.unlock()
        for item in snapshot {
            closeIO(item.replaceIO(nil))
        }
    }

    private func startNamespaceMonitor() {
        stopNamespaceMonitor()
        let timer = DispatchSource.makeTimerSource(queue: DispatchQueue(label: "vip.jstar.codexfold.fskit.namespace"))
        timer.schedule(deadline: .now() + .milliseconds(250), repeating: .milliseconds(500), leeway: .milliseconds(100))
        timer.setEventHandler { [weak self] in self?.refreshNamespace() }
        namespaceTimer = timer
        timer.resume()
    }

    private func stopNamespaceMonitor() {
        namespaceTimer?.cancel()
        namespaceTimer = nil
    }

    private func refreshNamespace() {
        guard let current = try? client.namespaceVersion() else { return }
        itemLock.lock()
        let previousVersion = namespaceVersion
        if current == namespaceVersion {
            itemLock.unlock()
            return
        }
        namespaceVersion = current
        let candidates = items.compactMap { nodeID, item -> (nodeID: UInt64, item: CodexFoldItem)? in
            item === rootItem ? nil : (nodeID, item)
        }
        itemLock.unlock()
        let candidatePaths = candidates.map { $0.item.entry.path }.sorted().joined(separator: ",")
        logger.info(
            "namespace refresh previous=\(previousVersion) current=\(current) candidates=\(candidates.count) paths=\(candidatePaths, privacy: .public)"
        )
        if let rootEntry = try? client.getattr("/") {
            rootItem.update(rootEntry)
        }

        var changedItems: [CodexFoldItem] = []
        var changedDirectories: [CodexFoldItem] = []
        var staleItems: [(nodeID: UInt64, item: CodexFoldItem)] = []
        for candidate in candidates {
            let item = candidate.item
            let previous = item.entry
            guard let refreshed = try? client.getattr(previous.path),
                  previous.hasSameObjectIdentity(as: refreshed) else {
                staleItems.append((candidate.nodeID, item))
                continue
            }
            if previous.type == .directory &&
                !previous.hasSameCachedDirectoryContents(as: refreshed) {
                item.update(refreshed)
                changedDirectories.append(item)
                continue
            }
            if previous.type == .file && !previous.hasSameCachedFileData(as: refreshed) {
                changedItems.append(item)
            }
            item.update(refreshed)
        }
        let changedDirectoryPaths = changedDirectories.map { $0.entry.path }.sorted().joined(separator: ",")
        logger.info(
            "namespace delta directories=\(changedDirectories.count) directory_paths=\(changedDirectoryPaths, privacy: .public) files=\(changedItems.count) stale=\(staleItems.count)"
        )

        var removedItems: [CodexFoldItem] = []
        itemLock.lock()
        for stale in staleItems where items[stale.nodeID] === stale.item {
            items.removeValue(forKey: stale.nodeID)
            removedItems.append(stale.item)
        }
        itemLock.unlock()
        for item in changedDirectories {
            if let error = setCacheState(
                for: item,
                cacheMode: .none,
                coherencyType: .noCache,
                // The directory still exists. Revoke is reserved for an item
                // that disappeared; invalidate asks the kernel to discard its
                // cached directory state without invalidating the live vnode.
                action: .invalidate
            ) {
                logger.error("directory contents cache invalidation failed: \(String(describing: error), privacy: .public)")
            }
        }
        for item in changedItems {
            item.invalidateReadCache()
            if let error = setCacheState(
                for: item,
                cacheMode: .none,
                coherencyType: .noCache,
                // A plain data-cache invalidation does not evict stale vnode
                // attributes after an external file-size change.
                action: .revoke
            ) {
                logger.error("external change cache revoke failed: \(String(describing: error), privacy: .public)")
            }
        }
        for item in removedItems {
            item.invalidateReadCache()
            if let error = setCacheState(
                for: item,
                cacheMode: .none,
                coherencyType: .noCache,
                action: .revoke
            ) {
                logger.error("external removal cache revoke failed: \(String(describing: error), privacy: .public)")
            }
        }
    }

    private func attributes(for entry: WireEntry) -> FSItem.Attributes {
        let result = FSItem.Attributes()
        result.uid = entry.uid
        result.gid = entry.gid
        result.linkCount = 1
        result.fileID = FSItem.Identifier(rawValue: entry.nodeID)!
        result.parentID = FSItem.Identifier(rawValue: entry.parentID)!
        result.mode = entry.mode
        result.type = itemType(entry.type)
        result.size = entry.type == .directory ? 0 : entry.size
        result.allocSize = entry.type == .directory ? 0 : entry.allocSize
        result.modifyTime = entry.modifyTime
        result.changeTime = entry.changeTime
        result.accessTime = entry.accessTime
        return result
    }

    private func freeSpaceSnapshot() -> FSFreeSpace {
        guard let stat = try? client.statfs() else {
            return FSFreeSpace.noUpdate
        }
        let freeSpace = FSFreeSpace()
        freeSpace.populate(bytes: stat.availableBytes)
        return freeSpace
    }

    private func itemType(_ type: WireEntryType) -> FSItem.ItemType {
        switch type {
        case .file: return .file
        case .directory: return .directory
        case .symlink: return .symlink
        case .unknown: return .unknown
        }
    }

    private func join(_ directory: String, _ name: String) -> String {
        if directory == "/" { return "/" + name }
        return directory + "/" + name
    }
}
