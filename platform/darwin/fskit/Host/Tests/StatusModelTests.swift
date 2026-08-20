import Darwin
import Foundation
import XCTest

final class CodexFoldRuntimeLocationTests: XCTestCase {
    func testDefaultRuntimeUsesAppGroupRoot() {
        let root = URL(fileURLWithPath: "/tmp/group.vip.jstar.codexfold", isDirectory: true)
        XCTAssertEqual(CodexFoldRuntimeLocation.rootURL(appGroupURL: root, environment: [:]), root)
    }

    func testScopedRuntimeUsesDirectAppGroupChild() {
        let root = URL(fileURLWithPath: "/tmp/group.vip.jstar.codexfold", isDirectory: true)
        let resolved = CodexFoldRuntimeLocation.rootURL(
            appGroupURL: root,
            environment: [CodexFoldRuntimeLocation.scopeEnvironmentKey: "acceptance-a38b89e5"]
        )
        XCTAssertEqual(resolved, root.appendingPathComponent("acceptance-a38b89e5", isDirectory: true))
    }

    func testInvalidRuntimeScopeFailsClosed() {
        let root = URL(fileURLWithPath: "/tmp/group.vip.jstar.codexfold", isDirectory: true)
        for scope in ["", ".", "..", "nested/scope", "../outside", " scope"] {
            XCTAssertNil(CodexFoldRuntimeLocation.rootURL(
                appGroupURL: root,
                environment: [CodexFoldRuntimeLocation.scopeEnvironmentKey: scope]
            ))
        }
    }
}

final class IncidentTrackerTests: XCTestCase {
    func testUnknownMissingAndStaleHealthyNeverRecoverIncident() throws {
        let start = Date(timeIntervalSince1970: 1_000)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let failure = status(
            health: .failed,
            updatedAt: start,
            incidentID: "incident-a",
            incidentSince: start,
            reason: "backend unavailable"
        )

        guard case let .active(_, isNew)? = tracker.evaluate(
            frontend: failure,
            supervisor: nil,
            at: start.addingTimeInterval(10)
        ) else {
            return XCTFail("expected a new frontend incident")
        }
        XCTAssertTrue(isNew)

        XCTAssertNil(tracker.evaluate(frontend: nil, supervisor: nil, at: start.addingTimeInterval(11)))
        XCTAssertNotNil(tracker.currentIncident)
        XCTAssertNil(tracker.evaluate(
            frontend: status(health: .unknown, updatedAt: nil),
            supervisor: nil,
            at: start.addingTimeInterval(12)
        ))
        XCTAssertNotNil(tracker.currentIncident)

        let staleHealthy = status(
            health: .healthy,
            updatedAt: start.addingTimeInterval(-1),
            incidentID: "incident-a"
        )
        XCTAssertNil(tracker.evaluate(
            frontend: staleHealthy,
            supervisor: nil,
            at: start.addingTimeInterval(13)
        ))
        XCTAssertNotNil(tracker.currentIncident)

        let unrelatedHealthy = status(
            health: .healthy,
            updatedAt: start.addingTimeInterval(14),
            incidentID: "incident-b"
        )
        XCTAssertNil(tracker.evaluate(
            frontend: unrelatedHealthy,
            supervisor: nil,
            at: start.addingTimeInterval(14)
        ))
        XCTAssertNotNil(tracker.currentIncident)

        let matchingHealthy = status(
            health: .healthy,
            updatedAt: start.addingTimeInterval(15),
            incidentID: "incident-a"
        )
        guard case .recovered? = tracker.evaluate(
            frontend: matchingHealthy,
            supervisor: nil,
            at: start.addingTimeInterval(15)
        ) else {
            return XCTFail("expected causally newer matching healthy evidence to recover")
        }
        XCTAssertNil(tracker.currentIncident)
    }

    func testSameIncidentRefreshIsNotNewAndNextIncidentIsNew() {
        let start = Date(timeIntervalSince1970: 2_000)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let initial = status(
            health: .failed,
            updatedAt: start,
            incidentID: "incident-a",
            incidentSince: start,
            reason: "initial error"
        )
        XCTAssertNil(tracker.evaluate(
            frontend: initial,
            supervisor: nil,
            at: start
        ))
        guard case let .active(_, firstIsNew)? = tracker.evaluate(
            frontend: initial,
            supervisor: nil,
            at: start.addingTimeInterval(10)
        ) else {
            return XCTFail("expected first incident")
        }
        XCTAssertTrue(firstIsNew)

        let updated = status(
            health: .failed,
            updatedAt: start.addingTimeInterval(11),
            incidentID: "incident-a",
            incidentSince: start,
            reason: "final transport error"
        )
        guard case let .active(incident, updateIsNew)? = tracker.evaluate(
            frontend: updated,
            supervisor: nil,
            at: start.addingTimeInterval(11)
        ) else {
            return XCTFail("expected active incident content update")
        }
        XCTAssertFalse(updateIsNew)
        XCTAssertEqual(incident.reason, L10n.text(.unknownReason))
        XCTAssertEqual(incident.technicalDetails, "final transport error")

        _ = tracker.evaluate(
            frontend: status(
                health: .healthy,
                updatedAt: start.addingTimeInterval(12),
                incidentID: "incident-a"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(12)
        )
        let second = status(
            health: .failed,
            updatedAt: start.addingTimeInterval(13),
            incidentID: "incident-b",
            incidentSince: start.addingTimeInterval(13)
        )
        XCTAssertNil(tracker.evaluate(
            frontend: second,
            supervisor: nil,
            at: start.addingTimeInterval(13)
        ))
        guard case let .active(_, secondIsNew)? = tracker.evaluate(
            frontend: second,
            supervisor: nil,
            at: start.addingTimeInterval(23)
        ) else {
            return XCTFail("expected second incident")
        }
        XCTAssertTrue(secondIsNew)
    }

    func testFailedStatusStillRequiresTenContinuousSeconds() {
        let start = Date(timeIntervalSince1970: 2_500)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let failure = status(
            health: .failed,
            updatedAt: start,
            incidentID: "incident-ten-second-contract",
            incidentSince: start
        )

        XCTAssertNil(tracker.evaluate(frontend: failure, supervisor: nil, at: start))
        XCTAssertNil(tracker.evaluate(
            frontend: failure,
            supervisor: nil,
            at: start.addingTimeInterval(9.999)
        ))
        guard case let .active(_, isNew)? = tracker.evaluate(
            frontend: failure,
            supervisor: nil,
            at: start.addingTimeInterval(10)
        ) else {
            return XCTFail("expected one incident at the exact ten-second threshold")
        }
        XCTAssertTrue(isNew)
        XCTAssertNil(tracker.evaluate(
            frontend: failure,
            supervisor: nil,
            at: start.addingTimeInterval(11)
        ))
    }

    func testFailureRecoveredBeforeTenSecondsNeverPresentsIncident() {
        let start = Date(timeIntervalSince1970: 2_600)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let failure = status(
            health: .failed,
            updatedAt: start,
            incidentID: "incident-short",
            incidentSince: start
        )
        XCTAssertNil(tracker.evaluate(frontend: failure, supervisor: nil, at: start))
        XCTAssertNil(tracker.evaluate(
            frontend: status(
                health: .healthy,
                updatedAt: start.addingTimeInterval(5),
                incidentID: "incident-short"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(5)
        ))
        XCTAssertNil(tracker.currentIncident)
    }

    func testFreshSupervisorRecoverySurvivesTransientReadFailureAndTriggersAtTenSeconds() {
        let start = Date(timeIntervalSince1970: 3_000)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let oldFrontendHealthy = status(
            health: .healthy,
            updatedAt: start.addingTimeInterval(-100)
        )

        XCTAssertNil(tracker.evaluate(
            frontend: oldFrontendHealthy,
            supervisor: status(id: "supervisor", health: .recovering, updatedAt: start),
            at: start
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            supervisor: nil,
            at: start.addingTimeInterval(3)
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: oldFrontendHealthy,
            supervisor: status(
                id: "supervisor",
                health: .recovering,
                updatedAt: start.addingTimeInterval(5)
            ),
            at: start.addingTimeInterval(5)
        ))

        guard case let .active(_, isNew)? = tracker.evaluate(
            frontend: oldFrontendHealthy,
            supervisor: status(
                id: "supervisor",
                health: .recovering,
                updatedAt: start.addingTimeInterval(11)
            ),
            at: start.addingTimeInterval(11)
        ) else {
            return XCTFail("expected supervisor-backed liveness incident")
        }
        XCTAssertTrue(isNew)

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            supervisor: status(
                id: "supervisor",
                health: .healthy,
                updatedAt: start.addingTimeInterval(12)
            ),
            at: start.addingTimeInterval(12)
        ))
        XCTAssertNotNil(tracker.currentIncident, "supervisor healthy alone is not frontend recovery evidence")

        guard case .recovered? = tracker.evaluate(
            frontend: status(health: .healthy, updatedAt: start.addingTimeInterval(10.5)),
            supervisor: status(
                id: "supervisor",
                health: .healthy,
                updatedAt: start.addingTimeInterval(13)
            ),
            at: start.addingTimeInterval(13)
        ) else {
            return XCTFail("expected fresh frontend and supervisor healthy evidence to recover")
        }
    }

    func testStaleSupervisorSnapshotDoesNotStartLivenessTimer() {
        let start = Date(timeIntervalSince1970: 4_000)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            supervisor: status(id: "supervisor", health: .recovering, updatedAt: start),
            at: start.addingTimeInterval(20)
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            supervisor: status(
                id: "supervisor",
                health: .recovering,
                updatedAt: start.addingTimeInterval(20)
            ),
            at: start.addingTimeInterval(20)
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            supervisor: status(
                id: "supervisor",
                health: .recovering,
                updatedAt: start.addingTimeInterval(25)
            ),
            at: start.addingTimeInterval(25)
        ))
        guard case .active? = tracker.evaluate(
            frontend: nil,
            supervisor: status(
                id: "supervisor",
                health: .recovering,
                updatedAt: start.addingTimeInterval(31)
            ),
            at: start.addingTimeInterval(31)
        ) else {
            return XCTFail("expected ten seconds of fresh supervisor recovery observations")
        }
    }

    func testDurableSupervisorOutageSurvivesUIAndHelperRestart() {
        let now = Date(timeIntervalSince1970: 4_400)
        let since = now.addingTimeInterval(-12)
        let oldFrontendHealthy = status(health: .healthy, updatedAt: now.addingTimeInterval(-100))
        let recovering = status(
            id: "supervisor",
            schemaVersion: 2,
            health: .recovering,
            updatedAt: now,
            incidentID: "supervisor-durable-outage",
            incidentSince: since,
            observationSequence: 40,
            publisherInstanceID: "supervisor-publisher-restarted",
            backendID: "supervisor-backend-a",
            mountPoint: "/managed/sessions",
            resourcePath: "/group/native-fskit"
        )

        for _ in 0..<2 {
            var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
            guard case let .active(incident, isNew)? = tracker.evaluate(
                frontend: oldFrontendHealthy,
                supervisor: recovering,
                at: now
            ) else {
                return XCTFail("durable twelve-second supervisor outage should survive presenter restart")
            }
            XCTAssertTrue(isNew)
            XCTAssertEqual(incident.id, "supervisor-durable-outage")
            XCTAssertEqual(incident.since, since)
        }
    }

    func testManagedIncidentRequiresNewerMatchingManagedRecoveryEvidence() {
        let start = Date(timeIntervalSince1970: 4_500)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let managedFailure = status(
            id: "managed",
            health: .failed,
            updatedAt: start,
            incidentID: "managed-incident",
            incidentSince: start.addingTimeInterval(-10),
            reason: "managed reload failed"
        )

        guard case let .active(incident, isNew)? = tracker.evaluate(
            frontend: nil,
            managed: managedFailure,
            supervisor: nil,
            at: start
        ) else {
            return XCTFail("expected managed incident")
        }
        XCTAssertTrue(isNew)
        XCTAssertEqual(incident.id, "managed-incident")

        XCTAssertNil(tracker.evaluate(
            frontend: status(
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                incidentID: "managed-incident"
            ),
            managed: nil,
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ), "frontend evidence must not recover a managed incident")

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(2),
                incidentID: "different-incident"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(2)
        ), "a different managed incident ID must not recover the incident")

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start,
                incidentID: "managed-incident"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(3)
        ), "stale matching evidence must not recover the incident")

        guard case .recovered? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(4),
                incidentID: "managed-incident"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(4)
        ) else {
            return XCTFail("expected newer matching managed evidence to recover")
        }
        XCTAssertNil(tracker.currentIncident)
    }

    func testLaterHealthySequenceOnSameBackendRecoversMissedManagedTransitions() {
        let start = Date(timeIntervalSince1970: 4_550)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let mountPoint = "/managed/sessions"
        let resourcePath = "/group/native-fskit"

        guard case .active? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .failed,
                updatedAt: start,
                incidentID: "incident-a",
                incidentSince: start.addingTimeInterval(-10),
                observationSequence: 100,
                mountPoint: mountPoint,
                resourcePath: resourcePath
            ),
            supervisor: nil,
            at: start
        ) else {
            return XCTFail("expected incident A")
        }

        // Healthy A and the complete B failure/recovery transition were not read by
        // the Host. The later sequence on the same backend proves both are behind us.
        guard case .recovered? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(4),
                incidentID: "incident-b",
                incidentSince: start.addingTimeInterval(2),
                observationSequence: 104,
                mountPoint: mountPoint,
                resourcePath: resourcePath
            ),
            supervisor: nil,
            at: start.addingTimeInterval(4)
        ) else {
            return XCTFail("expected later healthy sequence on the same backend to recover A")
        }
    }

    func testDifferentBackendOrOldSequenceCannotRecoverManagedIncident() {
        let start = Date(timeIntervalSince1970: 4_575)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let failure = status(
            id: "managed",
            health: .failed,
            updatedAt: start,
            incidentID: "incident-a",
            incidentSince: start.addingTimeInterval(-10),
            observationSequence: 200,
            mountPoint: "/managed/sessions",
            resourcePath: "/group/native-fskit"
        )
        _ = tracker.evaluate(frontend: nil, managed: failure, supervisor: nil, at: start)

		XCTAssertNil(tracker.evaluate(
			frontend: nil,
			managed: status(
				id: "managed",
				health: .healthy,
				updatedAt: start.addingTimeInterval(0.5),
				incidentID: "incident-a",
				incidentSince: start,
				observationSequence: 201,
				mountPoint: "/other/sessions",
				resourcePath: "/group/other-native-fskit"
			),
			supervisor: nil,
			at: start.addingTimeInterval(0.5)
		), "a matching incident ID from another backend must not recover")
		XCTAssertNotNil(tracker.currentIncident)

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                incidentID: "incident-b",
                incidentSince: start.addingTimeInterval(1),
                observationSequence: 201,
                mountPoint: "/other/sessions",
                resourcePath: "/group/other-native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ))
        XCTAssertNotNil(tracker.currentIncident)

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(2),
                incidentID: "incident-b",
                incidentSince: start.addingTimeInterval(1),
                observationSequence: 200,
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(2)
        ))
        XCTAssertNotNil(tracker.currentIncident)
    }

    func testManagedRecoveringContinuesToDeadlineWhenStatusObservationDisappears() {
        let start = Date(timeIntervalSince1970: 4_590)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .recovering,
                updatedAt: start,
                incidentID: "managed-pending",
                incidentSince: start,
                observationSequence: 300,
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: nil,
            supervisor: nil,
            at: start.addingTimeInterval(9)
        ))
        guard case let .active(incident, isNew)? = tracker.evaluate(
            frontend: nil,
            managed: nil,
            supervisor: nil,
            at: start.addingTimeInterval(10)
        ) else {
            return XCTFail("expected the preserved managed incident at its original deadline")
        }
        XCTAssertTrue(isNew)
        XCTAssertEqual(incident.id, "managed-pending")
        XCTAssertEqual(incident.since, start)
    }

    func testConcurrentCriticalIncidentsStaySingleAndSwitchAfterCurrentRecovery() {
        let start = Date(timeIntervalSince1970: 4_600)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let frontendFailure = status(
            health: .failed,
            updatedAt: start,
            incidentID: "frontend-incident",
            incidentSince: start.addingTimeInterval(-10),
            reason: "frontend failed"
        )
        let managedFailure = status(
            id: "managed",
            health: .failed,
            updatedAt: start,
            incidentID: "managed-incident",
            incidentSince: start.addingTimeInterval(-10),
            reason: "managed reload failed"
        )

        guard case let .active(first, firstIsNew)? = tracker.evaluate(
            frontend: frontendFailure,
            managed: managedFailure,
            supervisor: nil,
            at: start
        ) else {
            return XCTFail("expected the first critical incident")
        }
        XCTAssertTrue(firstIsNew)
        XCTAssertEqual(first.id, "frontend-incident")
        XCTAssertNil(tracker.evaluate(
            frontend: frontendFailure,
            managed: managedFailure,
            supervisor: nil,
            at: start.addingTimeInterval(0.5)
        ), "a concurrent source must not open a second active incident")

        guard case let .active(second, secondIsNew)? = tracker.evaluate(
            frontend: status(
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                incidentID: "frontend-incident"
            ),
            managed: nil,
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ) else {
            return XCTFail("expected the singleton incident to switch to managed")
        }
        XCTAssertTrue(secondIsNew)
        XCTAssertEqual(second.id, "managed-incident")
        XCTAssertEqual(tracker.currentIncident?.id, "managed-incident")

        guard case .recovered? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(2),
                incidentID: "managed-incident"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(2)
        ) else {
            return XCTFail("expected the queued managed incident to recover")
        }
    }

    func testManagedRecoveringTriggersAfterTenSeconds() {
        let start = Date(timeIntervalSince1970: 4_700)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .recovering,
                updatedAt: start,
                incidentID: "managed-recovery",
                incidentSince: start
            ),
            supervisor: nil,
            at: start
        ))
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .recovering,
                updatedAt: start.addingTimeInterval(9),
                incidentID: "managed-recovery",
                incidentSince: start
            ),
            supervisor: nil,
            at: start.addingTimeInterval(9)
        ))
        guard case let .active(incident, isNew)? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .recovering,
                updatedAt: start.addingTimeInterval(10),
                incidentID: "managed-recovery",
                incidentSince: start
            ),
            supervisor: nil,
            at: start.addingTimeInterval(10)
        ) else {
            return XCTFail("expected managed recovery incident at ten seconds")
        }
        XCTAssertTrue(isNew)
        XCTAssertEqual(incident.id, "managed-recovery")
    }

    func testAnonymousManagedHealthyCannotRecoverAnonymousFailure() {
        let start = Date(timeIntervalSince1970: 4_800)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)

        guard case .active? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .failed,
                updatedAt: start,
                incidentSince: start.addingTimeInterval(-10)
            ),
            supervisor: nil,
            at: start
        ) else {
            return XCTFail("expected anonymous managed failure to remain visible")
        }
        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                health: .healthy,
                updatedAt: start.addingTimeInterval(1)
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ), "recovery requires an explicit matching incident ID")
        XCTAssertNotNil(tracker.currentIncident)
    }

    func testManagedV2SameEpochRequiresStrictlyNewerSequenceWithoutTimestampDowngrade() {
        let start = Date(timeIntervalSince1970: 4_900)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        let failure = status(
            id: "managed",
            schemaVersion: 2,
            health: .failed,
            updatedAt: start,
            incidentID: "incident-v2-a",
            incidentSince: start.addingTimeInterval(-10),
            observationSequence: 100,
            publisherInstanceID: "publisher-a",
            backendID: "backend-a",
            mountPoint: "/managed/sessions",
            resourcePath: "/group/native-fskit"
        )
        guard case .active? = tracker.evaluate(
            frontend: nil,
            managed: failure,
            supervisor: nil,
            at: start
        ) else {
            return XCTFail("expected schema-v2 managed incident")
        }

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                incidentID: "incident-v2-a",
                incidentSince: start,
                observationSequence: 100,
                publisherInstanceID: "publisher-a",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ), "an equal sequence must not recover")
        XCTAssertNotNil(tracker.currentIncident)

        XCTAssertNil(tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(2),
                incidentID: "incident-v2-a",
                incidentSince: start,
                observationSequence: nil,
                publisherInstanceID: nil,
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(2)
        ), "schema-v2 evidence must not downgrade from sequence to time")
        XCTAssertNotNil(tracker.currentIncident)

        guard case .recovered? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(3),
                incidentID: "incident-v2-b",
                incidentSince: start.addingTimeInterval(2.5),
                observationSequence: 101,
                publisherInstanceID: "publisher-a",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(3)
        ) else {
            return XCTFail("a later healthy sequence in the same epoch must recover missed transitions")
        }
    }

    func testManagedV2NewPublisherEpochRecoversOnlyFreshHealthySameBackend() {
        let start = Date(timeIntervalSince1970: 5_000)
        let failure = status(
            id: "managed",
            schemaVersion: 2,
            health: .failed,
            updatedAt: start,
            incidentID: "incident-old-epoch",
            incidentSince: start.addingTimeInterval(-10),
            observationSequence: 9_000,
            publisherInstanceID: "publisher-old",
            backendID: "backend-a",
            mountPoint: "/managed/sessions",
            resourcePath: "/group/native-fskit"
        )

        var freshTracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        _ = freshTracker.evaluate(frontend: nil, managed: failure, supervisor: nil, at: start)
        guard case .recovered? = freshTracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                observationSequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ) else {
            return XCTFail("fresh healthy evidence from a new publisher epoch must recover the same backend")
        }

        var staleTracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        _ = staleTracker.evaluate(frontend: nil, managed: failure, supervisor: nil, at: start)
        XCTAssertNil(staleTracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                observationSequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(20)
        ), "stale evidence from another publisher epoch must not recover")
        XCTAssertNotNil(staleTracker.currentIncident)

        var otherBackendTracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        _ = otherBackendTracker.evaluate(frontend: nil, managed: failure, supervisor: nil, at: start)
        XCTAssertNil(otherBackendTracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                observationSequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-b",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ), "a new publisher for another backend must not recover")
        XCTAssertNotNil(otherBackendTracker.currentIncident)

        var inconsistentPathTracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        _ = inconsistentPathTracker.evaluate(frontend: nil, managed: failure, supervisor: nil, at: start)
        XCTAssertNil(inconsistentPathTracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(1),
                observationSequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a",
                mountPoint: "/other/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        ), "a status whose strong backend ID conflicts with its paths must not recover")
        XCTAssertNotNil(inconsistentPathTracker.currentIncident)
    }

    func testManagedV2NewEpochFailureMovesSequenceBaselineWithoutRecovering() {
        let start = Date(timeIntervalSince1970: 5_100)
        var tracker = IncidentTracker(timeout: 10, supervisorFreshness: 8)
        _ = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .failed,
                updatedAt: start,
                incidentID: "incident-restarted",
                incidentSince: start.addingTimeInterval(-10),
                observationSequence: 500,
                publisherInstanceID: "publisher-old",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start
        )

        let failureUpdate = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .recovering,
                updatedAt: start.addingTimeInterval(1),
                incidentID: "incident-restarted",
                incidentSince: start,
                observationSequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(1)
        )
        if case .recovered? = failureUpdate {
            XCTFail("a recovering status from a new publisher epoch must not recover")
        }
        XCTAssertNotNil(tracker.currentIncident)

        guard case .recovered? = tracker.evaluate(
            frontend: nil,
            managed: status(
                id: "managed",
                schemaVersion: 2,
                health: .healthy,
                updatedAt: start.addingTimeInterval(2),
                incidentID: "incident-restarted",
                incidentSince: start,
                observationSequence: 2,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a",
                mountPoint: "/managed/sessions",
                resourcePath: "/group/native-fskit"
            ),
            supervisor: nil,
            at: start.addingTimeInterval(2)
        ) else {
            return XCTFail("healthy sequence after the restarted failure baseline must recover")
        }
    }
}

final class DiagnosticRedactionTests: XCTestCase {
    func testDiagnosticPayloadOmitsRawDetailsAndRedactsPathsURLsAndCredentials() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-status-tests-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let payload: [String: Any] = [
            "schemaVersion": 1,
            "component": "frontend",
            "state": "failed",
            "updatedAt": "2026-07-25T10:00:00.000Z",
            "summary": "Failure in /Volumes/Private/Project",
            "detail": "raw detail at /private/tmp/codexfold.sock with token=raw-secret",
        ]
        let data = try JSONSerialization.data(withJSONObject: payload)
        try data.write(to: statusDirectory.appendingPathComponent("frontend.json"), options: .atomic)

        let incident = FrontendIncident(
            id: "incident-diagnostic",
            since: Date(timeIntervalSince1970: 5_000),
            reason: "Backend at /Volumes/Private/Project failed",
            impact: "See https://private.example.test/session?id=123",
            recommendations: ["Use Bearer top-secret-token only in the private console"],
            technicalDetails: "technical /Users/private/.codex/session.json",
            recoveredAt: nil
        )
        let diagnostic = await MainActor.run { () -> Data? in
            let store = StatusStore(appGroupURL: root)
            store.refresh()
            return store.diagnosticPayload(incident: incident)
        }
        let output = try XCTUnwrap(diagnostic.flatMap { String(data: $0, encoding: .utf8) })

        XCTAssertFalse(output.contains("/Volumes/"))
        XCTAssertFalse(output.contains("/private/"))
        XCTAssertFalse(output.contains("/Users/"))
        XCTAssertFalse(output.contains("https://"))
        XCTAssertFalse(output.contains("top-secret-token"))
        XCTAssertFalse(output.contains("raw-secret"))
        XCTAssertFalse(output.contains("\"detail\""))
        XCTAssertFalse(output.contains("technical_details"))
        XCTAssertTrue(output.contains("<redacted-path>"))
        XCTAssertTrue(output.contains("<redacted-url>"))
        XCTAssertTrue(output.contains("\"local_authorization\""))
        XCTAssertTrue(output.contains("\"sudo_cache\" : \"not_probed\""))
        XCTAssertTrue(output.contains("\"automatic_elevation\" : false"))
        XCTAssertTrue(output.contains("\"explicit_user_action_required\" : true"))
        XCTAssertTrue(output.contains("\"occurrence_continuity\" : \"verified_or_publisher\""))
    }
}

final class StatusHistoryTests: XCTestCase {
    func testStorageMetricsPreferTheStoragePublisherAndCalculateSavings() {
        let metrics = StorageMetrics.current(from: [
            status(
                id: "managed",
                health: .healthy,
                updatedAt: nil,
                managedSessions: 12,
                logicalBytes: 999,
                physicalBytes: 998
            ),
            status(
                id: "storage",
                health: .healthy,
                updatedAt: nil,
                logicalBytes: 400,
                physicalBytes: 100
            ),
        ])
        XCTAssertEqual(metrics?.managedSessions, 12)
        XCTAssertEqual(metrics?.logicalBytes, 400)
        XCTAssertEqual(metrics?.physicalBytes, 100)
        XCTAssertEqual(metrics?.savedBytes, 300)
        XCTAssertEqual(metrics?.savingsFraction, 0.75)
    }

    func testStorageHistoryReplacesTheCurrentMinuteAndCompactsOlderSamples() {
        let now = Date(timeIntervalSince1970: 40 * 24 * 60 * 60)
        var archive = StatusHistoryArchive()
        let first = StorageMetrics(managedSessions: 1, logicalBytes: 100, physicalBytes: 50)
        let updated = StorageMetrics(managedSessions: 2, logicalBytes: 120, physicalBytes: 30)

        XCTAssertTrue(archive.recordStorage(first, health: .healthy, at: now))
        XCTAssertFalse(archive.recordStorage(updated, health: .healthy, at: now.addingTimeInterval(30)))
        XCTAssertEqual(archive.storageSamples.count, 1)
        XCTAssertEqual(archive.storageSamples.first?.physicalBytes, 30)

        for offset in stride(from: -31 * 24 * 60, through: 0, by: 10) {
            let capturedAt = now.addingTimeInterval(TimeInterval(offset * 60))
            _ = archive.recordStorage(first, health: .healthy, at: capturedAt)
        }
        _ = archive.recordStorage(first, health: .healthy, at: now.addingTimeInterval(60))
        let cutoff = now.addingTimeInterval(-StatusHistoryArchive.retention)
        XCTAssertTrue(archive.storageSamples.allSatisfy { $0.capturedAt >= cutoff })
        XCTAssertLessThan(archive.storageSamples.count, 2_000)
    }

    func testIncidentHistoryKeepsOneOccurrenceAndAddsItsRecovery() {
        let since = Date(timeIntervalSince1970: 5_000)
        let recoveredAt = since.addingTimeInterval(18)
        let active = FrontendIncident(
            id: "incident-history-a",
            since: since,
            reason: "reason",
            impact: "impact",
            recommendations: [],
            technicalDetails: "",
            recoveredAt: nil
        )
        var recovered = active
        recovered.recoveredAt = recoveredAt
        var archive = StatusHistoryArchive()

        XCTAssertTrue(archive.recordIncident(.active(active, isNew: true), at: since))
        XCTAssertFalse(archive.recordIncident(.active(active, isNew: false), at: since.addingTimeInterval(1)))
        XCTAssertTrue(archive.recordIncident(.recovered(recovered), at: recoveredAt))
        XCTAssertEqual(archive.incidents.count, 1)
        XCTAssertEqual(archive.incidents.first?.recoveredAt, recoveredAt)
        XCTAssertEqual(archive.incidents.first?.duration(relativeTo: recoveredAt), 18)
    }

    func testStatusStoreReadsPublishedStorageAndPersistsAHistoryPoint() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-storage-history-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let capturedAt = Date(timeIntervalSince1970: 10_000)
        let formatter = ISO8601DateFormatter()
        let payload: [String: Any] = [
            "schemaVersion": 2,
            "component": "storage",
            "state": "healthy",
            "updatedAt": formatter.string(from: capturedAt),
            "logical_bytes": 400,
            "physical_bytes": 100,
        ]
        try JSONSerialization.data(withJSONObject: payload).write(
            to: statusDirectory.appendingPathComponent("storage.json"),
            options: .atomic
        )

        try await MainActor.run {
            let store = StatusStore(appGroupURL: root, now: { capturedAt })
            store.refresh()
            XCTAssertEqual(store.storageMetrics?.savedBytes, 300)
            XCTAssertEqual(store.menuBarTitle, "↓75%")
            XCTAssertEqual(store.storageHistory.count, 1)
        }
        XCTAssertTrue(FileManager.default.fileExists(
            atPath: root.appendingPathComponent("ui-history-v1.json").path
        ))
    }

    func testStatusStoreCalculatesRealReadAndWriteRatesFromCumulativeCounters() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-io-history-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let storagePayload: [String: Any] = [
            "schemaVersion": 2,
            "component": "storage",
            "state": "healthy",
            "updatedAt": "1970-01-01T02:46:40Z",
            "logical_bytes": 400,
            "physical_bytes": 100,
        ]
        try JSONSerialization.data(withJSONObject: storagePayload).write(
            to: statusDirectory.appendingPathComponent("storage.json"),
            options: .atomic
        )
        let activityURL = statusDirectory.appendingPathComponent("daemon.json")
        let startedAt = Date(timeIntervalSince1970: 10_000)

        try await MainActor.run {
            var current = startedAt
            try writeActivityStatus(
                to: activityURL,
                at: current,
                readBytes: 100,
                writtenBytes: 50
            )
            let store = StatusStore(appGroupURL: root, now: { current })
            store.refresh()
            XCTAssertNil(store.currentActivity)

            current = startedAt.addingTimeInterval(2)
            try writeActivityStatus(
                to: activityURL,
                at: current,
                readBytes: 500,
                writtenBytes: 250
            )
            store.refresh()
            XCTAssertEqual(store.currentActivity?.readBytesPerSecond, 200)
            XCTAssertEqual(store.currentActivity?.writtenBytesPerSecond, 100)
            XCTAssertEqual(store.storageHistory.last?.readBytesPerSecond, 200)
            XCTAssertEqual(store.storageHistory.last?.writtenBytesPerSecond, 100)
        }
    }
}

final class StatusStoreIntegrationTests: XCTestCase {
    func testStandaloneMenuBarLaunchDoesNotReportMissingRuntimeAsAnIncident() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let start = Date(timeIntervalSince1970: 5_900)

        try await MainActor.run {
            var current = start
            let store = StatusStore(
                appGroupURL: root,
                incidentTimeout: 10,
                monitorDaemonStatus: true,
                alertsBeforeRuntimeObserved: false,
                now: { current }
            )
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }

            store.refresh()
            current = start.addingTimeInterval(30)
            store.refresh()

            XCTAssertTrue(updates.isEmpty)
            XCTAssertNil(store.currentIncident)
            XCTAssertEqual(store.overallHealth, .unknown)
            XCTAssertTrue(store.isRuntimeUnconnected)

            current = start.addingTimeInterval(31)
            try writeManagedStatus(
                to: root.appendingPathComponent("status/managed.json"),
                state: "healthy",
                updatedAt: current,
                sequence: 1
            )
            store.refresh()
            XCTAssertFalse(store.isRuntimeUnconnected)
        }
    }

    func testFIFOStatusIsRejectedWithoutBlockingMainActor() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-status-fifo-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }
        XCTAssertEqual(Darwin.mkfifo(managedURL.path, S_IRUSR | S_IWUSR), 0)

        await MainActor.run {
            let store = StatusStore(appGroupURL: root)
            let started = Date()
            store.refresh()
            XCTAssertLessThan(Date().timeIntervalSince(started), 0.25)
            XCTAssertEqual(store.components.first(where: { $0.id == "managed" })?.health, .unknown)
        }
    }

    func testMalformedFrontendStatusDoesNotEmitRecoveryOrClearFailedHealth() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-status-corruption-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let frontendURL = statusDirectory.appendingPathComponent("frontend.json")
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let startedAt = Date()
        let now = ISO8601DateFormatter().string(from: startedAt)
        let failure: [String: Any] = [
            "schemaVersion": 1,
            "component": "frontend",
            "state": "unavailable",
            "updatedAt": now,
            "incidentID": "incident-corruption",
            "recoveryStartedAt": now,
            "summary": "Frontend unavailable",
        ]
        try JSONSerialization.data(withJSONObject: failure).write(to: frontendURL, options: .atomic)

        try await MainActor.run {
            var current = startedAt
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            XCTAssertTrue(updates.isEmpty)
            current = startedAt.addingTimeInterval(10)
            store.refresh()
            XCTAssertEqual(updates.count, 1)
            guard case .active? = updates.first else {
                return XCTFail("expected initial active incident")
            }

            try Data("{".utf8).write(to: frontendURL, options: .atomic)
            store.refresh()
            XCTAssertEqual(updates.count, 1, "corrupt status must not emit recovered")
            XCTAssertEqual(store.overallHealth, .failed)
        }
    }

    func testManagedStatusIsLoadedAsIndependentCriticalComponent() async throws {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-managed-status-\(UUID().uuidString)", isDirectory: true)
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: root) }

        let formatter = ISO8601DateFormatter()
        let failureDate = Date().addingTimeInterval(-10)
        let healthyUpdatedAt = formatter.string(from: Date().addingTimeInterval(-1))
        let recoveryStartedAt = formatter.string(from: failureDate)
        let failure: [String: Any] = [
            "schemaVersion": 1,
            "component": "managed",
            "state": "failed",
            "updatedAt": formatter.string(from: failureDate),
            "incidentID": "managed-store-incident",
            "recoveryStartedAt": formatter.string(from: failureDate),
            "summary": "Managed reload failed",
            "managedSessions": 7,
            "observationSequence": 1_000,
            "mountPoint": "/managed/sessions",
            "resourcePath": "/group/native-fskit",
        ]
        try JSONSerialization.data(withJSONObject: failure).write(to: managedURL, options: .atomic)

        try await MainActor.run {
            let store = StatusStore(appGroupURL: root)
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            XCTAssertEqual(store.components.first { $0.id == "managed" }?.health, .failed)
            XCTAssertEqual(store.components.first { $0.id == "managed" }?.managedSessions, 7)
            XCTAssertEqual(store.components.first { $0.id == "managed" }?.observationSequence, 1_000)
            XCTAssertEqual(store.components.first { $0.id == "managed" }?.mountPoint, "/managed/sessions")
            XCTAssertEqual(store.components.first { $0.id == "managed" }?.resourcePath, "/group/native-fskit")
            XCTAssertEqual(store.overallHealth, .failed)
            guard case let .active(incident, isNew)? = updates.first else {
                return XCTFail("expected managed status to emit an incident")
            }
            XCTAssertTrue(isNew)
            XCTAssertEqual(incident.id, "managed-store-incident")

            let healthy: [String: Any] = [
                "schemaVersion": 1,
                "component": "managed",
                "state": "healthy",
                "updatedAt": healthyUpdatedAt,
                "incidentID": "managed-store-incident",
                "recoveryStartedAt": recoveryStartedAt,
                "summary": "Managed sessions loaded",
                "observation_sequence": 1_001,
                "mount_point": "/managed/sessions",
                "resource_path": "/group/native-fskit",
            ]
            try JSONSerialization.data(withJSONObject: healthy).write(to: managedURL, options: .atomic)
            store.refresh()
            guard case .recovered? = updates.last else {
                return XCTFail("expected matching managed status to recover")
            }
        }
    }

    func testManagedStatusMissingFromStartupAlertsAtTenSeconds() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let start = Date(timeIntervalSince1970: 6_000)

        await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }

            store.refresh()
            current = start.addingTimeInterval(9)
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(10)
            store.refresh()
            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("expected missing managed status to alert at ten seconds")
            }
            XCTAssertTrue(isNew)
            XCTAssertTrue(incident.id.hasPrefix("managed-status-channel-"))
            XCTAssertEqual(incident.since, start)
            XCTAssertFalse(incident.recommendations.isEmpty)
        }
    }

    func testMalformedManagedStatusFromStartupAlertsAtTenSeconds() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        try Data("{".utf8).write(to: managedURL, options: .atomic)
        let start = Date(timeIntervalSince1970: 6_100)

        await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            current = start.addingTimeInterval(10)
            store.refresh()

            guard case let .active(incident, _)? = updates.last else {
                return XCTFail("expected malformed managed status to alert")
            }
            XCTAssertTrue(incident.id.hasPrefix("managed-status-channel-"))
            XCTAssertTrue(incident.technicalDetails.contains(L10n.text(.managedStatusChannelInvalidDetail)))
        }
    }

    func testIncompleteManagedV2StatusCannotDowngradeCausality() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        let start = Date(timeIntervalSince1970: 6_150)
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let incomplete: [String: Any] = [
            "schemaVersion": 2,
            "component": "managed",
            "state": "healthy",
            "updatedAt": formatter.string(from: start),
            "observationSequence": 1,
            "backendID": "backend-a",
            "mountPoint": "/managed/sessions",
            "resourcePath": "/group/native-fskit",
        ]
        try JSONSerialization.data(withJSONObject: incomplete).write(to: managedURL, options: .atomic)

        await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            current = start.addingTimeInterval(10)
            store.refresh()
            guard case let .active(incident, _)? = updates.last else {
                return XCTFail("expected incomplete schema-v2 status to remain an invalid channel")
            }
            XCTAssertTrue(incident.technicalDetails.contains(L10n.text(.managedStatusChannelInvalidDetail)))
        }
    }

    func testSymlinkManagedStatusCannotRecoverLocalStatusChannelIncident() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let external = root.appendingPathComponent("external-managed.json")
        let start = Date(timeIntervalSince1970: 6_200)
        try writeManagedStatus(
            to: external,
            state: "healthy",
            updatedAt: start.addingTimeInterval(11),
            sequence: 700
        )
        try FileManager.default.createSymbolicLink(at: managedURL, withDestinationURL: external)

        await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            current = start.addingTimeInterval(10)
            store.refresh()

            guard case .active? = updates.last else {
                return XCTFail("a symlink must remain an unavailable managed status channel")
            }
        }
    }

    func testManagedRecoveringStillAlertsAtOriginalDeadlineAfterStatusCorruption() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        let start = Date(timeIntervalSince1970: 6_300)
        try writeManagedStatus(
            to: managedURL,
            state: "recovering",
            updatedAt: start,
            sequence: 800,
            incidentID: "managed-original",
            recoveryStartedAt: start
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(5)
            try Data("{".utf8).write(to: managedURL, options: .atomic)
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(10)
            store.refresh()
            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("expected original managed recovery to reach its deadline")
            }
            XCTAssertTrue(isNew)
            XCTAssertEqual(incident.id, "managed-original")
            XCTAssertEqual(incident.since, start)
        }
    }

    func testStaleManagedSnapshotCannotRecoverButFreshSnapshotDoes() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        let start = Date(timeIntervalSince1970: 6_400)

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            current = start.addingTimeInterval(10)
            store.refresh()
            guard case let .active(active, _)? = updates.last else {
                return XCTFail("expected the local status-channel incident")
            }

            try writeManagedStatus(
                to: managedURL,
                state: "healthy",
                updatedAt: start.addingTimeInterval(-1),
                sequence: 900
            )
            current = start.addingTimeInterval(11)
            store.refresh()
            XCTAssertEqual(updates.count, 1, "stale status must not emit recovery")

            try writeManagedStatus(
                to: managedURL,
                state: "healthy",
                updatedAt: start.addingTimeInterval(12),
                sequence: 901
            )
            current = start.addingTimeInterval(12)
            store.refresh()
            guard case let .recovered(recovered)? = updates.last else {
                return XCTFail("expected a fresh regular managed snapshot to recover the channel")
            }
            XCTAssertEqual(recovered.id, active.id)
        }
    }

    func testManagedStatusThatStopsAdvancingAlertsAfterTenSeconds() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        let start = Date(timeIntervalSince1970: 6_500)
        try writeManagedStatus(
            to: managedURL,
            state: "healthy",
            updatedAt: start,
            sequence: 1_000
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            current = start.addingTimeInterval(2)
            store.refresh()
            current = start.addingTimeInterval(9)
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(10)
            store.refresh()
            guard case let .active(incident, _)? = updates.last else {
                return XCTFail("expected a non-advancing managed status report to alert")
            }
            XCTAssertEqual(incident.since, start)

            try writeManagedStatus(
                to: managedURL,
                state: "healthy",
                updatedAt: start.addingTimeInterval(11),
                sequence: 1_000
            )
            current = start.addingTimeInterval(11)
            store.refresh()
            XCTAssertEqual(updates.count, 1, "a rewritten but non-advancing sequence is not recovery")

            try writeManagedStatus(
                to: managedURL,
                state: "healthy",
                updatedAt: start.addingTimeInterval(12),
                sequence: 1_001
            )
            current = start.addingTimeInterval(12)
            store.refresh()
            guard case let .recovered(recovered)? = updates.last else {
                return XCTFail("expected a fresh advancing sequence to recover status monitoring")
            }
            XCTAssertEqual(recovered.id, incident.id)
        }
    }

    func testManagedStatusChannelAcceptsFreshSequenceResetFromNewPublisherEpoch() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let managedURL = root.appendingPathComponent("status/managed.json")
        let start = Date(timeIntervalSince1970: 6_600)
        try writeManagedStatus(
            to: managedURL,
            state: "healthy",
            updatedAt: start,
            sequence: 9_000,
            publisherInstanceID: "publisher-old",
            backendID: "backend-a"
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            try FileManager.default.removeItem(at: managedURL)
            current = start.addingTimeInterval(1)
            store.refresh()
            current = start.addingTimeInterval(11)
            store.refresh()
            guard case let .active(active, _)? = updates.last else {
                return XCTFail("expected status-channel outage before publisher restart")
            }

            try writeManagedStatus(
                to: managedURL,
                state: "healthy",
                updatedAt: start.addingTimeInterval(12),
                sequence: 1,
                publisherInstanceID: "publisher-new",
                backendID: "backend-a"
            )
            current = start.addingTimeInterval(12)
            store.refresh()
            guard case let .recovered(recovered)? = updates.last else {
                return XCTFail("expected a fresh new publisher epoch to recover the status channel")
            }
            XCTAssertEqual(recovered.id, active.id)
        }
    }

    func testSupervisorStatusMissingFromStartupAlertsAtTenSeconds() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json")
        let start = Date(timeIntervalSince1970: 6_700)
        try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 1)
        try FileManager.default.removeItem(at: supervisorURL)

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(9)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            try FileManager.default.removeItem(at: supervisorURL)
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            try FileManager.default.removeItem(at: supervisorURL)
            store.refresh()
            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("expected missing supervisor status to alert at ten seconds")
            }
            XCTAssertTrue(isNew)
            XCTAssertTrue(incident.id.hasPrefix("supervisor-status-channel-"))
            XCTAssertEqual(incident.since, start)
            XCTAssertTrue(incident.reason.contains("CodexFold"))
            XCTAssertFalse(incident.impact.isEmpty)
            XCTAssertFalse(incident.recommendations.isEmpty)
        }
    }

    func testSymlinkSupervisorStatusCannotSuppressStatusChannelIncident() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json")
        let externalURL = root.appendingPathComponent("external-supervisor.json")
        let start = Date(timeIntervalSince1970: 6_750)

        @Sendable func publish(at date: Date, sequence: UInt64) throws {
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: date, sequence: sequence)
            try FileManager.default.removeItem(at: supervisorURL)
            try writeSupervisorStatus(
                to: externalURL,
                state: "healthy",
                updatedAt: date,
                sequence: sequence
            )
            try FileManager.default.createSymbolicLink(at: supervisorURL, withDestinationURL: externalURL)
        }

        try publish(at: start, sequence: 1)
        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(10)
            try publish(at: current, sequence: 2)
            store.refresh()

            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("a symlink supervisor status must remain an unavailable status channel")
            }
            XCTAssertTrue(isNew)
            XCTAssertTrue(incident.id.hasPrefix("supervisor-status-channel-"))
            XCTAssertEqual(incident.since, start)
        }
    }

    func testUnexpectedStatusFileCannotImpersonateSupervisor() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json")
        let impostorURL = statusDirectory.appendingPathComponent("unrelated.json")
        let start = Date(timeIntervalSince1970: 6_775)

        @Sendable func publish(at date: Date, sequence: UInt64) throws {
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: date, sequence: sequence)
            try FileManager.default.removeItem(at: supervisorURL)
            try writeSupervisorStatus(
                to: impostorURL,
                state: "healthy",
                updatedAt: date,
                sequence: sequence
            )
        }

        try publish(at: start, sequence: 1)
        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(10)
            try publish(at: current, sequence: 2)
            store.refresh()

            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("a differently named status file must not impersonate supervisor.json")
            }
            XCTAssertTrue(isNew)
            XCTAssertTrue(incident.id.hasPrefix("supervisor-status-channel-"))
            XCTAssertEqual(incident.since, start)
        }
    }

    func testSupervisorStatusRecoveryRequiresAdvancingEpochAndMatchingBackendIdentity() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json")
        let start = Date(timeIntervalSince1970: 6_800)
        let publisher = "supervisor-publisher-old"
        let backend = "supervisor-backend-a"
        try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 100)
        try writeSupervisorStatus(
            to: supervisorURL,
            state: "healthy",
            updatedAt: start,
            sequence: 100,
            publisherInstanceID: publisher,
            backendID: backend
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 101)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "healthy",
                updatedAt: start,
                sequence: 100,
                publisherInstanceID: publisher,
                backendID: backend
            )
            store.refresh()
            guard case let .active(active, _)? = updates.last else {
                return XCTFail("expected a non-advancing supervisor status to alert")
            }

            current = start.addingTimeInterval(11)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 102)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "healthy",
                updatedAt: current,
                sequence: 100,
                publisherInstanceID: publisher,
                backendID: backend
            )
            store.refresh()
            XCTAssertEqual(updates.count, 1, "a rewritten timestamp with the same sequence is not recovery")

            current = start.addingTimeInterval(12)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 103)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "healthy",
                updatedAt: current,
                sequence: 1,
                publisherInstanceID: "supervisor-publisher-new",
                backendID: "supervisor-backend-b"
            )
            store.refresh()
            XCTAssertEqual(updates.count, 1, "another backend must not recover the supervisor incident")

            current = start.addingTimeInterval(13)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 104)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "healthy",
                updatedAt: current,
                sequence: 1,
                publisherInstanceID: "supervisor-publisher-new",
                backendID: backend
            )
            store.refresh()
            guard case let .recovered(recovered)? = updates.last else {
                return XCTFail("expected a fresh publisher epoch for the same backend to recover")
            }
            XCTAssertEqual(recovered.id, active.id)
        }
    }

    func testFreshSupervisorUnavailableAlertsAfterTenContinuousSeconds() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json")
        let start = Date(timeIntervalSince1970: 6_900)
        try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 1)
        try writeSupervisorStatus(
            to: supervisorURL,
            state: "unavailable",
            updatedAt: start,
            sequence: 1,
            detail: "foreign mount occupies the configured path"
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(9)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "unavailable",
                updatedAt: current,
                sequence: 2,
                detail: "foreign mount occupies the configured path"
            )
            store.refresh()
            XCTAssertTrue(updates.isEmpty)

            current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "unavailable",
                updatedAt: current,
                sequence: 3,
                detail: "foreign mount occupies the configured path"
            )
            store.refresh()
            guard case let .active(incident, isNew)? = updates.last else {
                return XCTFail("expected an explicit unavailable supervisor status to alert after ten seconds")
            }
            XCTAssertTrue(isNew)
            XCTAssertEqual(incident.reason, L10n.text(.supervisorRecoveryReason))
            XCTAssertEqual(incident.impact, L10n.text(.supervisorFailureImpact))
            XCTAssertTrue(incident.technicalDetails.contains("foreign mount"))

            current = start.addingTimeInterval(11)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 4)
            try writeSupervisorStatus(
                to: supervisorURL,
                state: "recovering",
                updatedAt: current,
                sequence: 4
            )
            store.refresh()
            guard case .recovered? = updates.last else {
                return XCTFail("expected newer ordered supervisor evidence to resolve the explicit failure")
            }
        }
    }

    func testMissingAndMalformedDaemonStatusAlertAfterTenSeconds() async throws {
        for malformed in [false, true] {
            let root = try managedStatusFixtureRoot()
            defer { try? FileManager.default.removeItem(at: root) }
            let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
            let managedURL = statusDirectory.appendingPathComponent("managed.json")
            let daemonURL = statusDirectory.appendingPathComponent("daemon.json")
            let start = Date(timeIntervalSince1970: malformed ? 7_100 : 7_000)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 1)
            if malformed { try Data("{".utf8).write(to: daemonURL, options: .atomic) }

            try await MainActor.run {
                var current = start
                let store = StatusStore(
                    appGroupURL: root,
                    monitorDaemonStatus: true,
                    now: { current }
                )
                var updates: [IncidentUpdate] = []
                store.onIncident = { updates.append($0) }
                store.refresh()
                current = start.addingTimeInterval(9)
                try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
                store.refresh()
                XCTAssertTrue(updates.isEmpty)

                current = start.addingTimeInterval(10)
                try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
                store.refresh()
                guard case let .active(incident, isNew)? = updates.last else {
                    return XCTFail("expected daemon status-channel incident at ten seconds")
                }
                XCTAssertTrue(isNew)
                XCTAssertEqual(incident.reason, L10n.text(.daemonStatusChannelReason))
            }
        }
    }

    func testStaleDaemonHeartbeatAlertsAndRequiresCausalRecovery() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let daemonURL = statusDirectory.appendingPathComponent("daemon.json")
        let start = Date(timeIntervalSince1970: 7_200)
        try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 1)
        try writeDaemonStatus(to: daemonURL, state: "healthy", updatedAt: start, sequence: 10)

        try await MainActor.run {
            var current = start
            let store = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()

            current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            store.refresh()
            guard case .active? = updates.last else {
                return XCTFail("expected a stale daemon heartbeat incident")
            }

            try writeDaemonStatus(to: daemonURL, state: "healthy", updatedAt: current, sequence: 10)
            store.refresh()
            XCTAssertEqual(updates.count, 1, "a rewritten timestamp without sequence progress is not recovery")

            try writeDaemonStatus(
                to: daemonURL,
                state: "healthy",
                updatedAt: current,
                sequence: 11,
                backendID: "daemon-backend-other"
            )
            store.refresh()
            XCTAssertEqual(updates.count, 1, "another logical backend cannot recover this outage")

            current = start.addingTimeInterval(11)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            try writeDaemonStatus(
                to: daemonURL,
                state: "healthy",
                updatedAt: current,
                sequence: 1,
                publisherInstanceID: "daemon-publisher-replacement"
            )
            store.refresh()
            guard case .recovered? = updates.last else {
                return XCTFail("expected a fresh publisher epoch for the same backend to recover")
            }
        }
    }

    func testOldExplicitDaemonFailurePresentsImmediatelyButNewFailureDoesNot() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let daemonURL = statusDirectory.appendingPathComponent("daemon.json")
        let start = Date(timeIntervalSince1970: 7_300)
        try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: start, sequence: 1)
        try writeDaemonStatus(
            to: daemonURL,
            state: "unavailable",
            updatedAt: start,
            sequence: 1,
            recoveryStartedAt: start.addingTimeInterval(-12)
        )

        try await MainActor.run {
            var current = start
            let store = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            guard case .active? = updates.last else {
                return XCTFail("publisher-proved twelve-second daemon outage should present immediately")
            }

            current = start.addingTimeInterval(13)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            try writeDaemonStatus(to: daemonURL, state: "healthy", updatedAt: current, sequence: 2)
            store.refresh()
            guard case .recovered? = updates.last else {
                return XCTFail("expected daemon recovery")
            }

            current = start.addingTimeInterval(14)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            try writeDaemonStatus(
                to: daemonURL,
                state: "unavailable",
                updatedAt: current,
                sequence: 3,
                recoveryStartedAt: current
            )
            store.refresh()
            XCTAssertEqual(updates.count, 2, "a new explicit failure must still wait ten seconds")
        }
    }

    func testDurableContinuitySeparatesANewOutageEvenWhenBothPresentersMissRecovery() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let daemonURL = statusDirectory.appendingPathComponent("daemon.json")
        let start = Date(timeIntervalSince1970: 7_400)
        try writeStatusContinuity(
            root: root,
            component: "daemon",
            epochID: "daemon-epoch-a",
            establishedAt: start,
            sequence: 10
        )

        try await MainActor.run {
            var current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 1)
            let firstStore = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var firstUpdates: [IncidentUpdate] = []
            firstStore.onIncident = { firstUpdates.append($0) }
            firstStore.refresh()
            guard case let .active(firstIncident, _)? = firstUpdates.last else {
                return XCTFail("expected the first durable missing-channel occurrence")
            }

            let acknowledgement = IncidentAcknowledgementStore(appGroupURL: root)
            try acknowledgement.acknowledge(firstIncident)

            current = start.addingTimeInterval(11)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            let restartedStore = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var restartedUpdates: [IncidentUpdate] = []
            restartedStore.onIncident = { restartedUpdates.append($0) }
            restartedStore.refresh()
            guard case let .active(sameIncident, _)? = restartedUpdates.last else {
                return XCTFail("expected the same outage after presenter restart")
            }
            XCTAssertEqual(sameIncident.id, firstIncident.id)
            XCTAssertTrue(acknowledgement.contains(sameIncident))

            let recoveredAt = start.addingTimeInterval(20)
            try writeDaemonStatus(
                to: daemonURL,
                state: "healthy",
                updatedAt: recoveredAt,
                sequence: 20,
                recoveryEpochID: "daemon-epoch-b",
                recoveryEpochEstablishedAt: recoveredAt
            )
            try writeStatusContinuity(
                root: root,
                component: "daemon",
                epochID: "daemon-epoch-b",
                establishedAt: recoveredAt,
                sequence: 20
            )
            try FileManager.default.removeItem(at: daemonURL)

            current = start.addingTimeInterval(31)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            let newStore = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var newUpdates: [IncidentUpdate] = []
            newStore.onIncident = { newUpdates.append($0) }
            newStore.refresh()
            guard case let .active(newIncident, _)? = newUpdates.last else {
                return XCTFail("expected a new outage derived from the newer healthy baseline")
            }
            XCTAssertNotEqual(newIncident.id, firstIncident.id)
            XCTAssertFalse(
                acknowledgement.contains(newIncident),
                "the old exact acknowledgement must not hide a new recovery epoch"
            )
        }
    }

    func testHealthyStatusCannotRecoverUntilItsContinuityCommitMatches() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let daemonURL = statusDirectory.appendingPathComponent("daemon.json")
        let start = Date(timeIntervalSince1970: 7_500)
        try writeStatusContinuity(
            root: root,
            component: "daemon",
            epochID: "daemon-epoch-old",
            establishedAt: start,
            sequence: 30
        )

        try await MainActor.run {
            var current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 1)
            let store = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            guard case .active? = updates.last else {
                return XCTFail("expected the original missing-channel occurrence")
            }

            current = start.addingTimeInterval(11)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            try writeDaemonStatus(
                to: daemonURL,
                state: "healthy",
                updatedAt: current,
                sequence: 31,
                recoveryEpochID: "daemon-epoch-new",
                recoveryEpochEstablishedAt: current
            )
            store.refresh()
            XCTAssertEqual(updates.count, 1, "main status alone cannot prove recovery before continuity commit")
            XCTAssertNotNil(store.currentIncident)

            try writeStatusContinuity(
                root: root,
                component: "daemon",
                epochID: "daemon-epoch-new",
                establishedAt: current,
                sequence: 31
            )
            store.refresh()
            guard case .recovered? = updates.last else {
                return XCTFail("matching durable continuity should complete recovery proof")
            }
        }
    }

    func testChannelRecoveryHandsOffToTheSameManagedOccurrenceWithoutANewWindow() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let start = Date(timeIntervalSince1970: 7_600)
        try writeStatusContinuity(
            root: root,
            component: "managed",
            epochID: "managed-epoch-a",
            establishedAt: start,
            sequence: 40
        )

        try await MainActor.run {
            var current = start.addingTimeInterval(10)
            let store = StatusStore(appGroupURL: root, now: { current })
            var updates: [IncidentUpdate] = []
            store.onIncident = { updates.append($0) }
            store.refresh()
            guard case let .active(channelIncident, firstIsNew)? = updates.last else {
                return XCTFail("expected the missing managed channel occurrence")
            }
            XCTAssertTrue(firstIsNew)

            current = start.addingTimeInterval(11)
            try writeManagedStatus(
                to: managedURL,
                state: "recovering",
                updatedAt: current,
                sequence: 41,
                incidentID: "managed-publisher-incident",
                recoveryStartedAt: start,
                recoveryEpochID: "managed-epoch-a",
                recoveryEpochEstablishedAt: start
            )
            store.refresh()
            guard case let .active(managedIncident, handoffIsNew)? = updates.last else {
                return XCTFail("expected the direct managed incident to take over")
            }
            XCTAssertEqual(managedIncident.id, channelIncident.id)
            XCTAssertFalse(handoffIsNew, "one continuous outage must not open a second window")
        }
    }

    func testUnavailableContinuityNeverLetsAnOldAcknowledgementSuppressAnotherPresenter() async throws {
        let root = try managedStatusFixtureRoot()
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let managedURL = statusDirectory.appendingPathComponent("managed.json")
        let continuityDirectory = root.appendingPathComponent("status-continuity", isDirectory: true)
        try FileManager.default.createDirectory(at: continuityDirectory, withIntermediateDirectories: true)
        try Data("{".utf8).write(
            to: continuityDirectory.appendingPathComponent("daemon.json"),
            options: .atomic
        )
        let start = Date(timeIntervalSince1970: 7_700)

        try await MainActor.run {
            var current = start
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 1)
            let firstStore = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var firstUpdates: [IncidentUpdate] = []
            firstStore.onIncident = { firstUpdates.append($0) }
            firstStore.refresh()
            current = start.addingTimeInterval(10)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 2)
            firstStore.refresh()
            guard case let .active(firstIncident, _)? = firstUpdates.last else {
                return XCTFail("expected an unverified-continuity incident")
            }
            let acknowledgement = IncidentAcknowledgementStore(appGroupURL: root)
            try acknowledgement.acknowledge(firstIncident)

            current = start.addingTimeInterval(20)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 3)
            let secondStore = StatusStore(
                appGroupURL: root,
                monitorDaemonStatus: true,
                now: { current }
            )
            var secondUpdates: [IncidentUpdate] = []
            secondStore.onIncident = { secondUpdates.append($0) }
            secondStore.refresh()
            current = start.addingTimeInterval(30)
            try writeManagedStatus(to: managedURL, state: "healthy", updatedAt: current, sequence: 4)
            secondStore.refresh()
            guard case let .active(secondIncident, _)? = secondUpdates.last else {
                return XCTFail("expected the replacement presenter to fail open to notification")
            }
            XCTAssertNotEqual(secondIncident.id, firstIncident.id)
            XCTAssertFalse(acknowledgement.contains(secondIncident))
        }
    }
}

private func managedStatusFixtureRoot() throws -> URL {
    let root = FileManager.default.temporaryDirectory
        .appendingPathComponent("codexfold-managed-channel-\(UUID().uuidString)", isDirectory: true)
    try FileManager.default.createDirectory(
        at: root.appendingPathComponent("status", isDirectory: true),
        withIntermediateDirectories: true
    )
    return root
}

private func writeManagedStatus(
    to url: URL,
    state: String,
    updatedAt: Date,
    sequence: UInt64,
    schemaVersion: Int = 2,
    publisherInstanceID: String = "managed-publisher-a",
    backendID: String = "managed-backend-a",
    incidentID: String? = nil,
    recoveryStartedAt: Date? = nil,
    recoveryEpochID: String? = nil,
    recoveryEpochEstablishedAt: Date? = nil,
    mountPoint: String = "/managed/sessions",
    resourcePath: String = "/group/native-fskit"
) throws {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    var payload: [String: Any] = [
        "schemaVersion": schemaVersion,
        "component": "managed",
        "state": state,
        "updatedAt": formatter.string(from: updatedAt),
        "summary": "Managed sessions \(state)",
        "managedSessions": 1,
        "observationSequence": NSNumber(value: sequence),
        "publisherInstanceID": publisherInstanceID,
        "backendID": backendID,
        "mountPoint": mountPoint,
        "resourcePath": resourcePath,
    ]
    if let incidentID {
        payload["incidentID"] = incidentID
    }
    if let recoveryStartedAt {
        payload["recoveryStartedAt"] = formatter.string(from: recoveryStartedAt)
    }
    if let recoveryEpochID {
        payload["recoveryEpochID"] = recoveryEpochID
    }
    if let recoveryEpochEstablishedAt {
        payload["recoveryEpochEstablishedAt"] = formatter.string(from: recoveryEpochEstablishedAt)
    }
    let data = try JSONSerialization.data(withJSONObject: payload, options: [.sortedKeys])
    try data.write(to: url, options: .atomic)
    if url.lastPathComponent == "managed.json" {
        try writeSupervisorStatus(
            to: url.deletingLastPathComponent().appendingPathComponent("supervisor.json"),
            state: "healthy",
            updatedAt: updatedAt,
            sequence: sequence,
            publisherInstanceID: "supervisor-\(publisherInstanceID)",
            backendID: "supervisor-\(backendID)"
        )
    }
}

private func writeSupervisorStatus(
    to url: URL,
    state: String,
    updatedAt: Date,
    sequence: UInt64,
    publisherInstanceID: String = "supervisor-publisher-a",
    backendID: String = "supervisor-backend-a",
    mountPoint: String = "/managed/sessions",
    resourcePath: String = "/group/native-fskit",
    detail: String = ""
) throws {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let payload: [String: Any] = [
        "schemaVersion": 2,
        "component": "supervisor",
        "state": state,
        "updatedAt": formatter.string(from: updatedAt),
        "summary": "File service supervision \(state)",
        "detail": detail,
        "observationSequence": NSNumber(value: sequence),
        "publisherInstanceID": publisherInstanceID,
        "backendID": backendID,
        "mountPoint": mountPoint,
        "resourcePath": resourcePath,
    ]
    let data = try JSONSerialization.data(withJSONObject: payload, options: [.sortedKeys])
    try data.write(to: url, options: .atomic)
}

private func writeActivityStatus(
    to url: URL,
    at observedAt: Date,
    readBytes: UInt64,
    writtenBytes: UInt64
) throws {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let payload: [String: Any] = [
        "schemaVersion": 2,
        "component": "daemon",
        "state": "healthy",
        "updatedAt": formatter.string(from: observedAt),
        "publisherInstanceID": "activity-publisher-test",
        "backendID": "daemon-backend-test",
        "mountPoint": "/managed/sessions",
        "resourcePath": "/group/native-fskit",
        "read_bytes_total": NSNumber(value: readBytes),
        "written_bytes_total": NSNumber(value: writtenBytes),
    ]
    try JSONSerialization.data(withJSONObject: payload).write(to: url, options: .atomic)
}

private func writeDaemonStatus(
    to url: URL,
    state: String,
    updatedAt: Date,
    sequence: UInt64,
    publisherInstanceID: String = "daemon-publisher-a",
    backendID: String = "daemon-backend-a",
    mountPoint: String = "/managed/sessions",
    resourcePath: String = "/group/native-fskit",
    recoveryStartedAt: Date? = nil,
    recoveryEpochID: String? = nil,
    recoveryEpochEstablishedAt: Date? = nil,
    detail: String = ""
) throws {
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    var payload: [String: Any] = [
        "schemaVersion": 2,
        "component": "daemon",
        "state": state,
        "updatedAt": formatter.string(from: updatedAt),
        "summary": "File service backend \(state)",
        "detail": detail,
        "observationSequence": NSNumber(value: sequence),
        "publisherInstanceID": publisherInstanceID,
        "backendID": backendID,
        "mountPoint": mountPoint,
        "resourcePath": resourcePath,
    ]
    if let recoveryStartedAt {
        payload["incidentID"] = "daemon-incident-a"
        payload["recoveryStartedAt"] = formatter.string(from: recoveryStartedAt)
    }
    if let recoveryEpochID {
        payload["recoveryEpochID"] = recoveryEpochID
    }
    if let recoveryEpochEstablishedAt {
        payload["recoveryEpochEstablishedAt"] = formatter.string(from: recoveryEpochEstablishedAt)
    }
    let data = try JSONSerialization.data(withJSONObject: payload, options: [.sortedKeys])
    try data.write(to: url, options: .atomic)
}

private func writeStatusContinuity(
    root: URL,
    component: String,
    epochID: String,
    establishedAt: Date,
    sequence: UInt64,
    backendID: String? = nil,
    mountPoint: String = "/managed/sessions",
    resourcePath: String = "/group/native-fskit"
) throws {
    let directory = root.appendingPathComponent("status-continuity", isDirectory: true)
    try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
    let formatter = ISO8601DateFormatter()
    formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let payload: [String: Any] = [
        "schemaVersion": 2,
        "component": component,
        "state": "healthy",
        "updatedAt": formatter.string(from: establishedAt),
        "generation": NSNumber(value: sequence),
        "publisherInstanceID": "\(component)-publisher-a",
        "observationSequence": NSNumber(value: sequence),
        "backendID": backendID ?? "\(component)-backend-a",
        "mountPoint": mountPoint,
        "resourcePath": resourcePath,
        "recoveryEpochID": epochID,
        "recoveryEpochEstablishedAt": formatter.string(from: establishedAt),
    ]
    let data = try JSONSerialization.data(withJSONObject: payload, options: [.sortedKeys])
    try data.write(
        to: directory.appendingPathComponent("\(component).json"),
        options: .atomic
    )
}

private func status(
    id: String = "frontend",
    schemaVersion: Int? = nil,
    health: ComponentHealth,
    updatedAt: Date?,
    incidentID: String? = nil,
    incidentSince: Date? = nil,
    reason: String? = nil,
    observationSequence: UInt64? = nil,
    publisherInstanceID: String? = nil,
    backendID: String? = nil,
    mountPoint: String? = nil,
    resourcePath: String? = nil,
    managedSessions: Int? = nil,
    logicalBytes: Int64? = nil,
    physicalBytes: Int64? = nil,
    readBytesTotal: UInt64? = nil,
    writtenBytesTotal: UInt64? = nil
) -> ComponentStatus {
    ComponentStatus(
        id: id,
        schemaVersion: schemaVersion,
        health: health,
        summary: id,
        detail: reason ?? "",
        updatedAt: updatedAt,
        incidentID: incidentID,
        incidentSince: incidentSince,
        reason: reason,
        impact: nil,
        recommendations: [],
        elapsedMilliseconds: nil,
        managedSessions: managedSessions,
        observationSequence: observationSequence,
        publisherInstanceID: publisherInstanceID,
        backendID: backendID,
        mountPoint: mountPoint,
        resourcePath: resourcePath,
        recoveryEpochID: nil,
        recoveryEpochEstablishedAt: nil,
        logicalBytes: logicalBytes,
        physicalBytes: physicalBytes,
        readBytesTotal: readBytesTotal,
        writtenBytesTotal: writtenBytesTotal
    )
}
