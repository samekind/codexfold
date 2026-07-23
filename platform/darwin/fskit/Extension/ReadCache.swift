import Darwin
import Foundation

struct CodexFoldReadAheadPolicy {
    private static let preferredReadAheadBytes = 12 * 1024 * 1024
    private static let fallbackReadAheadBytes = 11 * 1024 * 1024
    private static let maximumPrefetchBytes = 96 * 1024 * 1024
    private static let maximumConcurrentPrefetchCount = 8
    private static let maximumScheduledPrefetchCount = 8

    let readAheadBytes: Int
    let concurrentPrefetchCount: Int
    let scheduledPrefetchCount: Int
    let maxCachedBlocks: Int

    init(negotiatedReadBytes: Int) {
        if negotiatedReadBytes >= Self.preferredReadAheadBytes {
            readAheadBytes = Self.preferredReadAheadBytes
        } else {
            readAheadBytes = min(Self.fallbackReadAheadBytes, negotiatedReadBytes)
        }
        scheduledPrefetchCount = max(
            1,
            min(
                Self.maximumScheduledPrefetchCount,
                Self.maximumPrefetchBytes / max(1, readAheadBytes)
            )
        )
        concurrentPrefetchCount = min(
            Self.maximumConcurrentPrefetchCount,
            scheduledPrefetchCount
        )
        maxCachedBlocks = scheduledPrefetchCount + 1
    }
}

extension WireEntry {
    func hasSameObjectIdentity(as other: WireEntry) -> Bool {
        type == other.type &&
            path == other.path &&
            nodeID == other.nodeID
    }

    func hasSameCachedFileData(as other: WireEntry) -> Bool {
        type == .file &&
            other.type == .file &&
            hasSameObjectIdentity(as: other) &&
            size == other.size &&
            allocSize == other.allocSize &&
            modifyTime.tv_sec == other.modifyTime.tv_sec &&
            modifyTime.tv_nsec == other.modifyTime.tv_nsec &&
            changeTime.tv_sec == other.changeTime.tv_sec &&
            changeTime.tv_nsec == other.changeTime.tv_nsec
    }

    func hasSameCachedDirectoryContents(as other: WireEntry) -> Bool {
        type == .directory &&
            other.type == .directory &&
            hasSameObjectIdentity(as: other) &&
            contentGeneration == other.contentGeneration &&
            modifyTime.tv_sec == other.modifyTime.tv_sec &&
            modifyTime.tv_nsec == other.modifyTime.tv_nsec &&
            changeTime.tv_sec == other.changeTime.tv_sec &&
            changeTime.tv_nsec == other.changeTime.tv_nsec
    }
}

func writeRequiresKernelCacheInvalidation(
    previousSize: UInt64,
    offset: Int64,
    writtenBytes: Int,
    visibleSize: UInt64
) -> Bool {
    guard offset >= 0, writtenBytes >= 0 else { return true }
    let (literalEnd, overflow) = UInt64(offset).addingReportingOverflow(UInt64(writtenBytes))
    guard !overflow else { return true }
    return visibleSize != max(previousSize, literalEnd)
}

final class CodexFoldCachedReadBlock {
    let count: Int

    private enum Storage {
        case data(Data)
        case sharedWindow(WireSharedReadWindow, CodexFoldReadLease)
    }

    private let storage: Storage

    init(data: Data) {
        self.count = data.count
        self.storage = .data(data)
    }

    init(
        sharedWindow: WireSharedReadWindow,
        count: Int,
        release: @escaping () -> Void
    ) throws {
        guard count >= 0, count <= sharedWindow.capacity else {
            throw POSIXError(.EPROTO)
        }
        self.count = count
        self.storage = .sharedWindow(sharedWindow, CodexFoldReadLease(release: release))
    }

    func copyBytes(
        from sourceOffset: Int,
        count requestedCount: Int,
        to destination: UnsafeMutableRawPointer
    ) -> Bool {
        guard sourceOffset >= 0,
              requestedCount >= 0,
              sourceOffset <= count,
              requestedCount <= count - sourceOffset else {
            return false
        }
        guard requestedCount > 0 else { return true }

        switch storage {
        case .data(let data):
            _ = data.withUnsafeBytes { source in
                Darwin.memcpy(
                    destination,
                    source.baseAddress!.advanced(by: sourceOffset),
                    requestedCount
                )
            }
            return true
        case .sharedWindow(let window, _):
            return window.copyBytes(
                from: sourceOffset,
                count: requestedCount,
                to: destination
            )
        }
    }
}

private final class CodexFoldReadLease {
    private let release: () -> Void

    init(release: @escaping () -> Void) {
        self.release = release
    }

    deinit {
        release()
    }
}
