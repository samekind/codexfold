import XCTest

final class ReadAheadDecisionTests: XCTestCase {
    private let blockSize: Int64 = 12 * 1024 * 1024

    func testFirstTouchDoesNotExtendTheHorizon() {
        // Reading a rollout header is a lookup, not a scan. This is the case that
        // makes a session list cost a full store reread once every session is
        // pack backed.
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: nil,
                blockOffset: 0,
                blockSize: blockSize
            )
        )
    }

    func testRepeatedReadsInsideOneBlockDoNotExtendTheHorizon() {
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 0,
                blockOffset: 0,
                blockSize: blockSize
            )
        )
    }

    func testCrossingIntoTheNextBlockExtendsTheHorizon() {
        // A reader that works through one block and steps into the next is
        // scanning, and that is exactly when fetching ahead pays for itself.
        XCTAssertTrue(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 0,
                blockOffset: blockSize,
                blockSize: blockSize
            )
        )
        XCTAssertTrue(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 4 * blockSize,
                blockOffset: 5 * blockSize,
                blockSize: blockSize
            )
        )
    }

    func testJumpingToAnUnrelatedBlockDoesNotExtendTheHorizon() {
        // Random access gains nothing from read-ahead and pays the whole horizon
        // for it, so a jump forward is not treated as a scan either.
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 0,
                blockOffset: 5 * blockSize,
                blockSize: blockSize
            )
        )
    }

    func testSeekingBackwardsDoesNotExtendTheHorizon() {
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 5 * blockSize,
                blockOffset: 4 * blockSize,
                blockSize: blockSize
            )
        )
    }

    func testDegenerateBlockSizeAndOverflowAreRefused() {
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: 0,
                blockOffset: 0,
                blockSize: 0
            )
        )
        XCTAssertFalse(
            CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: Int64.max,
                blockOffset: Int64.min,
                blockSize: blockSize
            )
        )
    }

    func testAWholeFileScanOnlyMissesLookAheadForItsFirstBlock() {
        // Walk the blocks of a file the way a sequential reader does and count
        // how many of them got look-ahead scheduled.
        var previous: Int64?
        var extended = 0
        let blocks = 10
        for index in 0..<blocks {
            let offset = Int64(index) * blockSize
            if CodexFoldReadAheadDecision.extendsHorizon(
                previousBlockOffset: previous,
                blockOffset: offset,
                blockSize: blockSize
            ) {
                extended += 1
            }
            previous = offset
        }
        XCTAssertEqual(extended, blocks - 1, "only the first block of a scan goes without look-ahead")
    }
}
