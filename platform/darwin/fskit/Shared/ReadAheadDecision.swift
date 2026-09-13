import Foundation

/// Decides when a read is evidence of a scan rather than a lookup.
///
/// Read-ahead pays for itself only when the reader keeps going. Extending the
/// horizon on a handle's very first read charges every lookup the price of a
/// scan: a session list reads a few kilobytes of each rollout header, and with
/// an eight block horizon behind a twelve megabyte block that turns into
/// roughly a hundred megabytes of backend reads per file. Once every session is
/// pack backed that is the difference between listing sessions and rereading
/// the whole store.
enum CodexFoldReadAheadDecision {
    /// Whether a read of the block at `blockOffset` should extend the
    /// look-ahead horizon.
    ///
    /// Only a read that advances into the block immediately after the last one
    /// the handle touched counts. A first touch has no predecessor and proves
    /// nothing; a jump to an unrelated block is random access, which read-ahead
    /// cannot help either. Reads that stay inside the block already being
    /// served do not re-extend the horizon, so a reader working through one
    /// block in many small reads schedules its look-ahead once, when it
    /// actually crosses into the next block.
    static func extendsHorizon(
        previousBlockOffset: Int64?,
        blockOffset: Int64,
        blockSize: Int64
    ) -> Bool {
        guard blockSize > 0, let previousBlockOffset else { return false }
        guard previousBlockOffset <= Int64.max - blockSize else { return false }
        return blockOffset == previousBlockOffset + blockSize
    }
}
