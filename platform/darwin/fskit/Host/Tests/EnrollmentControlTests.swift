import Foundation
import XCTest

final class EnrollmentControlTests: XCTestCase {
    func testWaitingForLiveWriterIsNotErrorOrCompletedReclamation() {
        var progress = EnrollmentProgress.empty
        progress.enabled = true
        progress.phase = "waiting-reclaim"
        progress.managedCount = 3
        progress.waitingCount = 0
        progress.waitingKnown = true
        progress.cycleDone = 5
        progress.cycleTotal = 6
        XCTAssertFalse(progress.isWorking)
        XCTAssertEqual(progress.fraction, 5.0 / 6.0)
        assertCaption(progress, startsWith: .autoFoldReclaimWaiting, folded: 3, remaining: 0)
        XCTAssertTrue(progress.lastError.isEmpty)
    }
    func testPolicyRoundTripPreservesSettings() throws {
        let store = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-enrollment-policy-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: store, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: store) }

        let settings = EnrollmentSettings(
            enabled: true,
            interval: .twoHours,
            idleFor: .sixHours,
            archivedOnly: false,
            batchSize: .automatic
        )
        try EnrollmentPolicyFile.from(settings, preserving: nil).write(to: store)
        let loaded = try XCTUnwrap(EnrollmentPolicyFile.load(from: store))
        XCTAssertEqual(loaded.settings, settings)
        XCTAssertEqual(loaded.batchSize, 0, "automatic is stored as zero")
        XCTAssertEqual(loaded.interval, "2h")
        XCTAssertEqual(loaded.stableFor, "6h")
    }

    func testChangingOneSettingKeepsAnExactBatchThePaneCanOnlyRound() throws {
        let store = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-enrollment-batch-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: store, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: store) }

        // 137 is not one of the offered sizes; the pane shows the nearest.
        let tuned = EnrollmentPolicyFile(
            version: 1, enabled: true, interval: "30m", stableFor: "1h",
            archivedOnly: false, batchSize: 137
        )
        try tuned.write(to: store)
        let shown = try XCTUnwrap(EnrollmentPolicyFile.load(from: store)).settings
        XCTAssertEqual(shown.batchSize, .oneHundred)

        // Changing an unrelated setting must not retune a batch nobody touched.
        var settings = shown
        settings.idleFor = .sixHours
        let existing = EnrollmentPolicyFile.load(from: store)
        try EnrollmentPolicyFile.from(settings, preserving: existing).write(to: store)
        XCTAssertEqual(try XCTUnwrap(EnrollmentPolicyFile.load(from: store)).batchSize, 137)

        // Actually choosing a size writes that size.
        settings.batchSize = .fifty
        let current = EnrollmentPolicyFile.load(from: store)
        try EnrollmentPolicyFile.from(settings, preserving: current).write(to: store)
        XCTAssertEqual(try XCTUnwrap(EnrollmentPolicyFile.load(from: store)).batchSize, 50)
    }

    func testProgressCaptionAndFraction() {
        var progress = EnrollmentProgress.empty
        progress.enabled = true
        progress.phase = "idle"
        progress.managedCount = 4
        progress.waitingCount = 12
        progress.waitingKnown = true
        XCTAssertEqual(progress.fraction, 4.0 / 16.0)
        XCTAssertTrue(progress.caption(now: Date()).contains("4"))
        XCTAssertTrue(progress.caption(now: Date()).contains("12"))

        progress.phase = "checking"
        XCTAssertNil(progress.fraction)
        assertCaption(progress, startsWith: .autoFoldChecking, folded: 4, remaining: 12)

        progress.phase = "folding"
        progress.cycleTotal = 1
        progress.cycleDone = 0
        XCTAssertEqual(progress.fraction, 0)
        assertCaption(progress, startsWith: .autoFoldFolding, folded: 4, remaining: 12)

        progress = .empty
        XCTAssertEqual(progress.fraction, 0)
        XCTAssertEqual(progress.caption(now: Date()), L10n.text(.autoFoldOff))
    }

    func testRelativeCheckUsesWholeMinutes() {
        let now = Date(timeIntervalSince1970: 1_000)
        XCTAssertEqual(
            EnrollmentProgress.relativeCheck(from: now.addingTimeInterval(18 * 60), now: now),
            L10n.format(.autoFoldCheckMinutesFormat, 18)
        )
        XCTAssertEqual(
            EnrollmentProgress.relativeCheck(from: now.addingTimeInterval(-1), now: now),
            L10n.text(.autoFoldCheckSoon)
        )
    }

    func testIdleBeforeFirstScanDoesNotAnimateWork() {
        var progress = EnrollmentProgress.empty
        progress.enabled = true
        progress.phase = "idle"
        progress.managedCount = 4
        XCTAssertFalse(progress.waitingKnown)
        XCTAssertFalse(progress.isWorking)
        XCTAssertEqual(progress.fraction, 0)
        progress.phase = "checking"
        XCTAssertNil(progress.fraction)
    }

    func testReclamationRemainsWorkingUntilVerifiedComplete() {
        var progress = EnrollmentProgress.empty
        progress.enabled = true
        progress.phase = "reclaiming"
        progress.cycleTotal = 6
        progress.cycleDone = 5
        XCTAssertTrue(progress.isWorking)
        XCTAssertEqual(progress.fraction, 5.0 / 6.0)
        XCTAssertEqual(progress.caption(now: Date()), L10n.text(.autoFoldReclaiming))
    }

    @MainActor
    func testStatusStoreLoadsProgressAndWritesPolicy() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-enrollment-status-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let storeDir = root.appendingPathComponent("fold-store", isDirectory: true)
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        try FileManager.default.createDirectory(at: storeDir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let payload: [String: Any] = [
            "version": 1,
            "store_path": storeDir.path,
            "enabled": true,
            "interval": "30m0s",
            "stable_for": "1h0m0s",
            "archived_only": true,
            "phase": "idle",
            "managed_count": 4,
            "waiting_count": 12,
            "waiting_known": true,
            "cycle_total": 0,
            "cycle_done": 0,
            "next_check_at": "2026-08-31T09:00:00.000Z",
            "updated_at": "2026-08-31T08:30:00.000Z",
        ]
        try JSONSerialization.data(withJSONObject: payload).write(
            to: statusDirectory.appendingPathComponent("enrollment.json"),
            options: .atomic
        )

        let store = StatusStore(appGroupURL: root, recordsHistory: false, enrollmentStoreURL: storeDir)
        store.refresh()
        XCTAssertTrue(store.enrollmentSettings.enabled)
        XCTAssertEqual(store.enrollmentSettings.interval, .thirtyMinutes)
        XCTAssertEqual(store.enrollmentSettings.idleFor, .oneHour)
        XCTAssertTrue(store.enrollmentSettings.archivedOnly)
        XCTAssertEqual(store.enrollmentProgress.managedCount, 4)
        XCTAssertEqual(store.enrollmentProgress.waitingCount, 12)
        XCTAssertFalse(store.components.contains(where: { $0.id.lowercased() == "enrollment" }))

        store.setEnrollmentEnabled(false)
        store.setEnrollmentInterval(.oneHour)
        store.setEnrollmentIdleFor(.oneDay)
        store.setEnrollmentArchivedOnly(false)
        let policy = try XCTUnwrap(EnrollmentPolicyFile.load(from: storeDir))
        XCTAssertFalse(policy.enabled)
        XCTAssertEqual(policy.interval, "1h")
        XCTAssertEqual(policy.stableFor, "24h")
        XCTAssertFalse(policy.archivedOnly)
        XCTAssertEqual(policy.batchSize, 0)
    }

    func testProgressLoadAcceptsGoDurationStrings() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-enrollment-progress-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let url = root.appendingPathComponent("status.json")
        let payload = """
        {
          "version": 1,
          "store_path": "/tmp/fold-store",
          "enabled": false,
          "interval": "2h0m0s",
          "stable_for": "6h0m0s",
          "archived_only": false,
          "phase": "disabled",
          "managed_count": 1,
          "waiting_count": 0,
          "waiting_known": true,
          "cycle_total": 0,
          "cycle_done": 0,
          "updated_at": "2026-08-31T08:30:00Z"
        }
        """
        try Data(payload.utf8).write(to: url, options: .atomic)
        let progress = try XCTUnwrap(EnrollmentProgress.load(from: url))
        XCTAssertEqual(progress.storePath, "/tmp/fold-store")
        XCTAssertEqual(progress.phase, "disabled")
        XCTAssertEqual(EnrollmentCheckInterval.matching(progress.interval), .twoHours)
        XCTAssertEqual(EnrollmentIdleDuration.matching(progress.stableFor), .sixHours)
    }
}

private func assertCaption(
    _ progress: EnrollmentProgress,
    startsWith key: L10nKey,
    folded: Int,
    remaining: Int,
    file: StaticString = #filePath,
    line: UInt = #line
) {
    let caption = progress.caption(now: Date())
    XCTAssertTrue(caption.hasPrefix(L10n.text(key)), "caption=\(caption)", file: file, line: line)
    // The phase alone is not progress. A pass that is working must still say how
    // far along it is, otherwise a long run reads as a stalled one.
    XCTAssertTrue(caption.contains("\(folded)"), "caption=\(caption)", file: file, line: line)
    XCTAssertTrue(caption.contains("\(remaining)"), "caption=\(caption)", file: file, line: line)
}

final class EnrollmentBatchSizeTests: XCTestCase {
    func testAutomaticIsTheZeroSetting() {
        XCTAssertEqual(EnrollmentBatchSize.matching(0), .automatic)
        XCTAssertEqual(EnrollmentBatchSize.matching(-5), .automatic)
        XCTAssertEqual(EnrollmentBatchSize.automatic.rawValue, 0)
    }

    func testAChosenBatchRoundTripsThroughThePolicy() throws {
        let store = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-batch-choice-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: store, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: store) }

        for choice in EnrollmentBatchSize.allCases {
            let settings = EnrollmentSettings(
                enabled: true, interval: .thirtyMinutes, idleFor: .oneHour,
                archivedOnly: false, batchSize: choice
            )
            try EnrollmentPolicyFile.from(settings, preserving: nil).write(to: store)
            let loaded = try XCTUnwrap(EnrollmentPolicyFile.load(from: store))
            XCTAssertEqual(loaded.batchSize, choice.rawValue, "\(choice) did not survive a write")
            XCTAssertEqual(loaded.settings.batchSize, choice, "\(choice) did not survive a read")
        }
    }

    func testAValueThisBuildDoesNotOfferSnapsToTheNearestChoice() {
        // Whatever a future build or a hand-edited policy stores, the pane has to
        // show something, and the nearest offered size is closer to the user's
        // intent than silently reverting to automatic.
        XCTAssertEqual(EnrollmentBatchSize.matching(12), .ten)
        XCTAssertEqual(EnrollmentBatchSize.matching(60), .fifty)
        XCTAssertEqual(EnrollmentBatchSize.matching(1000), .twoHundred)
        XCTAssertEqual(EnrollmentBatchSize.matching(1), .ten)
    }
}
