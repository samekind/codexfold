#if CODEXFOLD_RECOVERY_TESTS
import Darwin
import Dispatch
import Foundation

@main
private enum BackendRecoveryTests {
    static func main() throws {
        try testDirectoryEnumerationContinuation()
        try testTransportClassification()
        try testConnectErrorClassification()
        try testWriteRecoveryDecisions()
        try testRecoveryRetriesBeforeSuccess()
        try testRecoveryRereadsDescriptor()
        try testConcurrentCallersShareRecovery()
        try testRecoveryDeadlineIsBounded()
        try testCoordinatorPublishesUnavailable()
        try testFrontendRuntimeScopeIsValidatedAndApplied()
        try testFrontendStatusIsAtomicallyPublished()
        try testFrontendStatusUsesProvidedRuntimeRoot()
        try testFrontendStatusPublicationReportsUnsafeDestination()
        print("BackendRecoveryTests: PASS")
    }

    private static func testDirectoryEnumerationContinuation() throws {
        let cache = DirectoryEnumerationCache<Int>()
        cache.store(node: 10, generation: 1, entries: [1, 2, 3])
        try require(cache.continuation(node: 10, generation: 1, start: 2) == [1, 2, 3], "continuation must reuse listing")
        try require(cache.continuation(node: 10, generation: 2, start: 2) == nil, "changed directory must not reuse listing")
        cache.store(node: 10, generation: 2, entries: [4])
        try require(cache.continuation(node: 10, generation: 2, start: 0) == nil, "new enumeration must reread even unchanged directory")
        try require(cache.continuation(node: 10, generation: 2, start: 1) == nil, "new enumeration invalidates previous snapshot")
        cache.store(node: 10, generation: 2, entries: Array(0...16_384))
        try require(cache.continuation(node: 10, generation: 2, start: 1) == nil, "oversized snapshots must not remain resident")
    }

    private static func testTransportClassification() throws {
        try require(isWireTransportError(POSIXError(.EPIPE)), "EPIPE must trigger recovery")
        try require(isWireTransportError(POSIXError(.ECONNRESET)), "ECONNRESET must trigger recovery")
        try require(!isWireTransportError(POSIXError(.EINVAL)), "EINVAL must not trigger recovery")
    }

    private static func testConnectErrorClassification() throws {
        // The backend unlinks its socket while restarting, so connect(2) fails
        // with ENOENT. Reporting that verbatim showed Codex file-not-found
        // during an ordinary restart; it must enter the recovery wait instead.
        try require(
            isWireTransportError(wireConnectError(ENOENT)),
            "a missing backend socket must trigger recovery, not file-not-found"
        )
        try require(
            wireConnectError(ENOENT).code != .ENOENT,
            "a missing backend socket must never surface as ENOENT"
        )
        // Unrelated connect failures keep their exact code, and a response
        // status of ENOENT never reaches this classifier, so a genuinely
        // missing file still fails immediately.
        try require(
            wireConnectError(EINVAL).code == .EINVAL,
            "an unrelated connect failure must keep its code"
        )
        try require(
            !isWireTransportError(wireConnectError(EINVAL)),
            "an unrelated connect failure must not trigger recovery"
        )
    }

    private static func testWriteRecoveryDecisions() throws {
        let request = Data("{\"ok\":true}\n".utf8)
        try require(
            wireWriteRecoveryDecision(
                existing: request,
                requested: request,
                supportsSnapshotReplay: true
            ) == .alreadyCommitted,
            "identical bytes must prove that a write committed"
        )
        try require(
            wireWriteRecoveryDecision(
                existing: request.prefix(4),
                requested: request,
                supportsSnapshotReplay: true
            ) == .replaySnapshot,
            "a matching snapshot prefix may be replayed"
        )
        try require(
            wireWriteRecoveryDecision(
                existing: request.prefix(4),
                requested: request,
                supportsSnapshotReplay: false
            ) == .conflict,
            "ordinary writes must not replay an uncertain prefix"
        )
        try require(
            wireWriteRecoveryDecision(
                existing: Data("DIFFERENT".utf8),
                requested: request,
                supportsSnapshotReplay: true
            ) == .conflict,
            "divergent bytes must stop recovery"
        )
    }

    private static func testRecoveryRetriesBeforeSuccess() throws {
        let coordinator = BackendRecoveryCoordinator(
            mountID: "test-mount",
            generation: 1,
            timeout: 0.5,
            retryDelay: 0.001,
            statusWriter: nil
        )
        var attempts = 0
        let generation = try coordinator.recover(
            noLaterThan: Date().addingTimeInterval(0.5),
            after: POSIXError(.ECONNRESET)
        ) { _ in
            attempts += 1
            if attempts < 3 {
                throw POSIXError(.ECONNREFUSED)
            }
            return 42
        }
        try require(generation == 42, "recovery must return the installed generation")
        try require(attempts == 3, "recovery must retry descriptor/connect failures")
    }

    private static func testRecoveryRereadsDescriptor() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "codexfold-descriptor-recovery-\(UUID().uuidString.lowercased())",
            isDirectory: true
        )
        defer { try? FileManager.default.removeItem(at: root) }
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        try writeDescriptor(generation: 1, to: root)
        let coordinator = BackendRecoveryCoordinator(
            mountID: "test-mount",
            generation: 1,
            timeout: 0.5,
            retryDelay: 0.001,
            statusWriter: nil
        )
        var observedGenerations: [UInt64] = []
        let generation = try coordinator.recover(
            noLaterThan: Date().addingTimeInterval(0.5),
            after: POSIXError(.ECONNRESET)
        ) { _ in
            let descriptor = try WireDescriptor(resourceURL: root)
            observedGenerations.append(descriptor.generation)
            if descriptor.generation == 1 {
                try writeDescriptor(generation: 2, to: root)
                throw POSIXError(.ECONNREFUSED)
            }
            return descriptor.generation
        }
        try require(generation == 2, "recovery must accept the refreshed descriptor generation")
        try require(observedGenerations == [1, 2], "each recovery attempt must reread descriptor.bin")
    }

    private static func testConcurrentCallersShareRecovery() throws {
        let coordinator = BackendRecoveryCoordinator(
            mountID: "test-mount",
            generation: 1,
            timeout: 1,
            retryDelay: 0.001,
            statusWriter: nil
        )
        let start = DispatchSemaphore(value: 0)
        let group = DispatchGroup()
        let lock = NSLock()
        var reconnects = 0
        var generations: [UInt64] = []
        var failures: [any Error] = []

        for _ in 0..<8 {
            group.enter()
            DispatchQueue.global(qos: .userInitiated).async {
                start.wait()
                do {
                    let generation = try coordinator.recover(
                        noLaterThan: Date().addingTimeInterval(1),
                        after: POSIXError(.EPIPE)
                    ) { _ in
                        lock.withLock { reconnects += 1 }
                        Thread.sleep(forTimeInterval: 0.1)
                        return 77
                    }
                    lock.withLock { generations.append(generation) }
                } catch {
                    lock.withLock { failures.append(error) }
                }
                group.leave()
            }
        }
        for _ in 0..<8 { start.signal() }
        try require(group.wait(timeout: .now() + 2) == .success, "concurrent recovery timed out")
        try require(lock.withLock { failures.isEmpty }, "all recovery waiters must succeed")
        try require(lock.withLock { reconnects == 1 }, "concurrent callers must share one reconnect")
        try require(lock.withLock { generations == Array(repeating: 77, count: 8) }, "waiters must receive one result")
    }

    private static func testRecoveryDeadlineIsBounded() throws {
        let coordinator = BackendRecoveryCoordinator(
            mountID: "test-mount",
            generation: 1,
            timeout: 0.05,
            retryDelay: 0.005,
            statusWriter: nil
        )
        let startedAt = Date()
        do {
            _ = try coordinator.recover(
                noLaterThan: startedAt.addingTimeInterval(0.05),
                after: POSIXError(.ECONNRESET)
            ) { _ in
                throw POSIXError(.ECONNREFUSED)
            }
            throw TestFailure("recovery unexpectedly succeeded")
        } catch is TestFailure {
            throw TestFailure("recovery unexpectedly succeeded")
        } catch {
            let elapsed = Date().timeIntervalSince(startedAt)
            try require(elapsed >= 0.04, "recovery stopped before its retry window")
            try require(elapsed < 0.2, "recovery exceeded its configured deadline")
        }
    }

    private static func testFrontendRuntimeScopeIsValidatedAndApplied() throws {
        let root = URL(fileURLWithPath: "/tmp/group.vip.jstar.codexfold", isDirectory: true)
        let scoped = FrontendRuntimeLocation.containerURL(
            appGroupURL: root,
            environment: ["CODEXFOLD_RUNTIME_SCOPE": "acceptance-a193842z"]
        )
        try require(
            scoped == root.appendingPathComponent("acceptance-a193842z", isDirectory: true),
            "frontend runtime scope must stay beneath the app-group root"
        )
        for invalid in ["", ".", "..", "nested/scope", "../outside", " scope"] {
            try require(
                FrontendRuntimeLocation.containerURL(
                    appGroupURL: root,
                    environment: ["CODEXFOLD_RUNTIME_SCOPE": invalid]
                ) == nil,
                "invalid frontend runtime scope must fail closed: \(invalid)"
            )
        }
    }

    private static func testFrontendStatusIsAtomicallyPublished() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "codexfold-frontend-status-\(UUID().uuidString.lowercased())",
            isDirectory: true
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let writer = FrontendStatusWriter(containerURL: root)
        let startedAt = Date(timeIntervalSince1970: 1_700_000_000)
        let deadline = startedAt.addingTimeInterval(10)
        writer.publish(FrontendRecoveryStatus(
            state: .recovering,
            updatedAt: startedAt,
            summary: "recovering",
            detail: "waiting",
            mountID: "test-mount",
            generation: 9,
            recoveryStartedAt: startedAt,
            recoveryDeadlineAt: deadline,
            elapsedMilliseconds: 125,
            lastTransportError: "POSIX 32",
            incidentID: "incident-test"
        ))
        writer.waitForPendingWrites()

        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        let destination = statusDirectory.appendingPathComponent("frontend.json")
        let data = try Data(contentsOf: destination)
        guard let payload = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw TestFailure("frontend status is not a JSON object")
        }
        let requiredKeys: Set<String> = [
            "schemaVersion", "component", "state", "updatedAt", "summary", "detail",
            "mountID", "generation", "recoveryStartedAt", "recoveryDeadlineAt",
            "elapsedMilliseconds", "lastTransportError", "incidentID",
        ]
        try require(requiredKeys.isSubset(of: Set(payload.keys)), "frontend status is missing required keys")
        try require(payload["component"] as? String == "frontend", "component must be frontend")
        try require(payload["state"] as? String == "recovering", "state must be recovering")
        let leftovers = try FileManager.default.contentsOfDirectory(atPath: statusDirectory.path)
            .filter { $0.hasSuffix(".tmp") }
        try require(leftovers.isEmpty, "atomic status publish left a temporary file")
    }

    private static func testFrontendStatusPublicationReportsUnsafeDestination() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "codexfold-frontend-status-unsafe-\(UUID().uuidString.lowercased())",
            isDirectory: true
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let statusDirectory = root.appendingPathComponent("status", isDirectory: true)
        try FileManager.default.createDirectory(at: statusDirectory, withIntermediateDirectories: true)
        let external = root.appendingPathComponent("external.json")
        try Data("preserve\n".utf8).write(to: external)
        try FileManager.default.createSymbolicLink(
            at: statusDirectory.appendingPathComponent("frontend.json"),
            withDestinationURL: external
        )
        var publicationError: (any Error)?
        let writer = FrontendStatusWriter(containerURL: root) { publicationError = $0 }
        writer.publish(FrontendRecoveryStatus(
            state: .unavailable,
            updatedAt: Date(),
            summary: "unavailable",
            detail: "test",
            mountID: "test-mount",
            generation: 1,
            recoveryStartedAt: Date(),
            recoveryDeadlineAt: Date().addingTimeInterval(10),
            elapsedMilliseconds: 10_000,
            lastTransportError: "test",
            incidentID: "incident-unsafe-destination"
        ))
        writer.waitForPendingWrites()
        try require(publicationError != nil, "unsafe frontend destination failure was swallowed")
        let externalContents = try Data(contentsOf: external)
        try require(
            externalContents == Data("preserve\n".utf8),
            "unsafe frontend destination modified the symlink target"
        )
    }

    private static func testFrontendStatusUsesProvidedRuntimeRoot() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "codexfold-frontend-status-scope-\(UUID().uuidString.lowercased())",
            isDirectory: true
        )
        let scoped = root.appendingPathComponent("acceptance-test", isDirectory: true)
        defer { try? FileManager.default.removeItem(at: root) }
        let writer = FrontendStatusWriter(containerURL: scoped)
        writer.publish(FrontendRecoveryStatus(
            state: .healthy,
            updatedAt: Date(),
            summary: "healthy",
            detail: "connected",
            mountID: "test-mount",
            generation: 1,
            recoveryStartedAt: nil,
            recoveryDeadlineAt: nil,
            elapsedMilliseconds: 0,
            lastTransportError: nil,
            incidentID: nil
        ))
        writer.waitForPendingWrites()

        let scopedStatus = scoped
            .appendingPathComponent("status", isDirectory: true)
            .appendingPathComponent("frontend.json", isDirectory: false)
        try require(
            FileManager.default.fileExists(atPath: scopedStatus.path),
            "frontend status was not written beneath the provided runtime root"
        )
        let unscopedStatus = root
            .appendingPathComponent("status", isDirectory: true)
            .appendingPathComponent("frontend.json", isDirectory: false)
        try require(
            !FileManager.default.fileExists(atPath: unscopedStatus.path),
            "frontend status escaped to the app-group root"
        )
    }

    private static func testCoordinatorPublishesUnavailable() throws {
        let root = FileManager.default.temporaryDirectory.appendingPathComponent(
            "codexfold-unavailable-status-\(UUID().uuidString.lowercased())",
            isDirectory: true
        )
        defer { try? FileManager.default.removeItem(at: root) }
        let writer = FrontendStatusWriter(containerURL: root)
        let coordinator = BackendRecoveryCoordinator(
            mountID: "test-mount",
            generation: 3,
            timeout: 0.02,
            retryDelay: 0.002,
            statusWriter: writer
        )
        do {
            _ = try coordinator.recover(
                noLaterThan: Date().addingTimeInterval(0.02),
                after: POSIXError(.ECONNRESET)
            ) { _ in
                throw POSIXError(.ECONNREFUSED)
            }
            throw TestFailure("unavailable recovery unexpectedly succeeded")
        } catch is TestFailure {
            throw TestFailure("unavailable recovery unexpectedly succeeded")
        } catch {
            writer.waitForPendingWrites()
        }
        let destination = root
            .appendingPathComponent("status", isDirectory: true)
            .appendingPathComponent("frontend.json")
        let data = try Data(contentsOf: destination)
        guard let payload = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
            throw TestFailure("unavailable status is not a JSON object")
        }
        try require(payload["state"] as? String == "unavailable", "deadline expiry must publish unavailable")
        try require(payload["incidentID"] as? String != nil, "unavailable status must retain its incident ID")
        try require(payload["recoveryStartedAt"] as? String != nil, "unavailable status must retain its start time")
    }

    private static func writeDescriptor(generation: UInt64, to directory: URL) throws {
        var writer = WireWriter()
        writer.raw(Data([0x43, 0x46, 0x53, 0x52]))
        writer.uint16(2)
        writer.uint16(0)
        writer.uint64(generation)
        writer.string("/tmp/codexfold-recovery-test.sock")
        writer.bytes(Data(repeating: UInt8(truncatingIfNeeded: generation), count: 16))
        try writer.data.write(
            to: directory.appendingPathComponent("descriptor.bin"),
            options: [.atomic]
        )
    }

    private static func require(_ condition: @autoclosure () -> Bool, _ message: String) throws {
        guard condition() else { throw TestFailure(message) }
    }
}

private struct TestFailure: Error, CustomStringConvertible {
    let description: String

    init(_ description: String) {
        self.description = description
    }
}
#endif
