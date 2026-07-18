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

private final class CodexFoldIO {
    private static let readAheadBytes = 1 * 1024 * 1024

    let connection: WireConnection
    let handle: UInt64
    let writable: Bool
    private let cacheLock = NSLock()
    private var readCacheOffset: Int64 = 0
    private var readCache = Data()

    init(connection: WireConnection, handle: UInt64, writable: Bool) {
        self.connection = connection
        self.handle = handle
        self.writable = writable
    }

    func read(client: DaemonClient, offset: Int64, length: Int) throws -> Data {
        guard offset >= 0, length >= 0 else { throw POSIXError(.EINVAL) }
        guard length > 0 else { return Data() }
        if writable {
            return try client.read(handle: handle, offset: offset, length: length, connection: connection)
        }
        if length >= Self.readAheadBytes {
            return try client.read(handle: handle, offset: offset, length: length, connection: connection)
        }

        cacheLock.lock()
        defer { cacheLock.unlock() }
        if offset >= readCacheOffset {
            let start = offset - readCacheOffset
            if start <= Int64(readCache.count), Int64(length) <= Int64(readCache.count) - start {
                let lower = Int(start)
                return readCache.subdata(in: lower..<(lower + length))
            }
        }

        let blockSize = Int64(Self.readAheadBytes)
        let fetchOffset = offset / blockSize * blockSize
        let requestedEnd = offset - fetchOffset + Int64(length)
        let fetchLength = Int(max(blockSize, requestedEnd))
        let fetched = try client.read(handle: handle, offset: fetchOffset, length: fetchLength, connection: connection)
        readCacheOffset = fetchOffset
        readCache = fetched

        let lower = Int(offset - fetchOffset)
        guard lower < fetched.count else { return Data() }
        return fetched.subdata(in: lower..<min(fetched.count, lower + length))
    }

    func invalidateReadCache() {
        cacheLock.lock()
        readCacheOffset = 0
        readCache.removeAll(keepingCapacity: false)
        cacheLock.unlock()
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
        lock.unlock()
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
        capabilities.supportsJournal = true
        capabilities.supportsActiveJournal = true
        capabilities.supportsSparseFiles = false
        capabilities.supportsFastStatFS = true
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
                    freeSpace: nil
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
                    freeSpace: nil
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
                    freeSpace: nil
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
            replyHandler(FSSetAttributesResult(attributes: attributes(for: entry), freeSpace: nil), nil)
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
            let entries = try client.readDir(directory.entry.path)
            let currentVersion = try client.namespaceVersion()
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
            guard let result = FSSetXattrResult(freeSpace: nil) else {
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
            let data = try io.read(client: client, offset: Int64(offset), length: length)
            _ = buffer.withUnsafeMutableBytes { destination in
                data.copyBytes(to: destination.bindMemory(to: UInt8.self))
            }
            let entry = item.entry
            replyHandler(FSReadFileResult(bytesRead: data.count, itemAttributes: attributes(for: entry)), nil)
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
			io.invalidateReadCache()
            let count = try client.write(handle: io.handle, offset: Int64(offset), data: contents, connection: io.connection)
            let entry = try client.getattr(item.entry.path)
            item.update(entry)
            replyHandler(FSWriteFileResult(bytesWritten: count, itemAttributes: attributes(for: entry), freeSpace: nil), nil)
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
            if item.entry.type == .file {
				_ = try ensureIO(item, writable: writable)
            }
			let coherency: FSVolume.KernelCacheCoherencyType = !writable && cacheMode != .none ? .readCache : .noCache
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
				_ = setCacheState(for: item, cacheMode: .none, coherencyType: .noCache, action: .revoke)
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
            if item.entry.type == .file {
				_ = try ensureIO(item, writable: writable)
            }
			let coherency: FSVolume.KernelCacheCoherencyType = writable ? .noCache : .readCache
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
            let handle = try client.open(item.entry.path, flags: flags, connection: connection)
            let io = CodexFoldIO(connection: connection, handle: handle, writable: writable)
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

        var valid: UInt32 = 0
        if request.isValid(.mode) { valid |= 1 << 0 }
        if request.isValid(.uid) { valid |= 1 << 1 }
        if request.isValid(.gid) { valid |= 1 << 2 }
        if request.isValid(.accessTime) { valid |= 1 << 3 }
        if request.isValid(.modifyTime) { valid |= 1 << 4 }
        guard valid != 0 else { return }

        try client.setAttributes(
            path,
            valid: valid,
            mode: request.mode,
            uid: request.uid,
            gid: request.gid,
            accessTime: request.accessTime,
            modifyTime: request.modifyTime
        )
        if request.isValid(.mode) { request.consumedAttributes.insert(.mode) }
        if request.isValid(.uid) { request.consumedAttributes.insert(.uid) }
        if request.isValid(.gid) { request.consumedAttributes.insert(.gid) }
        if request.isValid(.accessTime) { request.consumedAttributes.insert(.accessTime) }
        if request.isValid(.modifyTime) { request.consumedAttributes.insert(.modifyTime) }
    }

    private func closeIO(_ io: CodexFoldIO?) {
        guard let io else { return }
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
        if current == namespaceVersion {
            itemLock.unlock()
            return
        }
        namespaceVersion = current
        let staleItems = items.values.filter { $0 !== rootItem }
        items = [rootItem.entry.nodeID: rootItem]
        itemLock.unlock()
        if let rootEntry = try? client.getattr("/") {
            rootItem.update(rootEntry)
        }
        for item in staleItems {
			item.invalidateReadCache()
            _ = setCacheState(for: item, cacheMode: .none, coherencyType: .noCache, action: .revoke)
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
