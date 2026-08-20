import Foundation
import XCTest

final class IncidentAcknowledgementStoreTests: XCTestCase {
    func testAdministratorCapabilityUsesOnlyReadOnlyGroupEvidence() {
        XCTAssertEqual(
            LocalAuthorizationCapability.resolveAdministratorMembership(
                adminGroupID: 80,
                effectiveGroupID: 20,
                supplementaryGroups: [12, 80]
            ),
            .member
        )
        XCTAssertEqual(
            LocalAuthorizationCapability.resolveAdministratorMembership(
                adminGroupID: 80,
                effectiveGroupID: 20,
                supplementaryGroups: [12, 61]
            ),
            .notMember
        )
        let capability = LocalAuthorizationCapability(administratorMembership: .member)
        XCTAssertEqual(capability.sudoCacheState, "not_probed")
        XCTAssertFalse(capability.automaticElevationAllowed)
        XCTAssertTrue(capability.explicitUserActionRequired)
    }

    func testAcknowledgementMatchesOnlyTheExactDurableOccurrence() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let store = IncidentAcknowledgementStore(appGroupURL: root)
        let incident = makeIncident(id: "incident-a", since: Date(timeIntervalSince1970: 100))
        XCTAssertFalse(store.contains(incident))
        try store.acknowledge(incident)
        XCTAssertTrue(store.contains(incident))
        XCTAssertTrue(
            store.contains(
                FrontendIncident(
                    id: incident.id,
                    since: incident.since,
                    reason: "more precise backend failure",
                    impact: "more precise impact",
                    recommendations: incident.recommendations,
                    technicalDetails: incident.technicalDetails,
                    recoveredAt: nil
                )
            ),
            "detail refreshes for the same occurrence must not reopen the window"
        )
        XCTAssertFalse(
            store.contains(makeIncident(id: "incident-after-monitor-restart", since: incident.since.addingTimeInterval(1))),
            "a different durable occurrence must not be hidden by matching copy"
        )
        XCTAssertFalse(
            store.contains(makeIncident(id: incident.id, since: incident.since.addingTimeInterval(1))),
            "the same incident ID with a different start time is a new occurrence"
        )
        store.clear(matching: incident)
        XCTAssertFalse(store.contains(incident))
    }

    func testSyntheticAcknowledgementUsesTheExactOccurrenceIDAcrossPresenterRestart() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let store = IncidentAcknowledgementStore(appGroupURL: root)
        let start = Date(timeIntervalSince1970: 200)
        let synthesized = makeIncident(
            id: "daemon-status-channel-occurrence-a",
            since: start
        )
        try store.acknowledge(synthesized)
        XCTAssertTrue(
            store.contains(
                makeIncident(
                    id: synthesized.id,
                    since: start.addingTimeInterval(1)
                )
            ),
            "the same exact synthetic occurrence remains acknowledged across presenter restart"
        )
        XCTAssertFalse(
            store.contains(makeIncident(id: "daemon-status-channel-occurrence-b", since: start)),
            "a new recovery epoch must produce a new occurrence even when source and copy are unchanged"
        )

        let legacyPayload: [String: Any] = [
            "incidentID": "daemon-status-channel-old-process",
            "incidentSince": start.timeIntervalSince1970,
            "reason": synthesized.reason,
            "impact": synthesized.impact,
            "syntheticScope": "synthetic:daemon-status-channel-",
        ]
        let legacyData = try JSONSerialization.data(withJSONObject: legacyPayload)
        try DurableAppGroupFile.write(legacyData, to: store.url)
        XCTAssertFalse(
            store.contains(synthesized),
            "legacy source-wide synthetic acknowledgements cannot suppress an exact occurrence"
        )
    }

    func testPresentationLeaseTransfersAfterTheHolderReleasesIt() throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let first = IncidentPresentationLease(appGroupURL: root)
        let second = IncidentPresentationLease(appGroupURL: root)
        XCTAssertTrue(first.acquire())
        XCTAssertFalse(second.acquire())
        first.release()
        XCTAssertTrue(second.acquire())
    }

    private func makeIncident(id: String, since: Date) -> FrontendIncident {
        FrontendIncident(
            id: id,
            since: since,
            reason: "backend unavailable",
            impact: "session access may fail",
            recommendations: ["Wait for automatic recovery."],
            technicalDetails: "test",
            recoveredAt: nil
        )
    }
}
