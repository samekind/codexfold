import Combine
import CryptoKit
import Darwin
import Foundation

enum CodexFoldRuntimeLocation {
    static let scopeEnvironmentKey = "CODEXFOLD_RUNTIME_SCOPE"

    static func rootURL(
        appGroupURL: URL?,
        environment: [String: String] = ProcessInfo.processInfo.environment
    ) -> URL? {
        guard let appGroupURL else { return nil }
        guard let scope = environment[scopeEnvironmentKey] else { return appGroupURL }
        guard !scope.isEmpty,
              scope.count <= 64,
              scope != ".",
              scope != "..",
              scope.range(of: "^[A-Za-z0-9][A-Za-z0-9._-]*$", options: .regularExpression) != nil else {
            return nil
        }
        return appGroupURL.appendingPathComponent(scope, isDirectory: true)
    }
}

enum ComponentHealth: Int, Comparable, Codable {
    case healthy = 0
    case unknown = 1
    case recovering = 2
    case failed = 3

    static func < (lhs: ComponentHealth, rhs: ComponentHealth) -> Bool {
        lhs.rawValue < rhs.rawValue
    }
}

struct LocalAuthorizationCapability: Equatable {
    enum AdministratorMembership: String {
        case member
        case notMember = "not_member"
        case unknown
    }

    let administratorMembership: AdministratorMembership
    let sudoCacheState = "not_probed"
    let automaticElevationAllowed = false
    let explicitUserActionRequired = true

    static func current() -> LocalAuthorizationCapability {
        guard let adminGroup = Darwin.getgrnam("admin") else {
            return LocalAuthorizationCapability(administratorMembership: .unknown)
        }
        let adminGroupID = adminGroup.pointee.gr_gid
        let groupCount = Darwin.getgroups(0, nil)
        guard groupCount >= 0 else {
            return LocalAuthorizationCapability(administratorMembership: .unknown)
        }
        var supplementaryGroups = [gid_t](repeating: 0, count: Int(groupCount))
        if groupCount > 0 {
            let loaded = supplementaryGroups.withUnsafeMutableBufferPointer { buffer in
                Darwin.getgroups(groupCount, buffer.baseAddress)
            }
            guard loaded >= 0 else {
                return LocalAuthorizationCapability(administratorMembership: .unknown)
            }
            supplementaryGroups.removeSubrange(Int(loaded)..<supplementaryGroups.count)
        }
        return LocalAuthorizationCapability(
            administratorMembership: resolveAdministratorMembership(
                adminGroupID: adminGroupID,
                effectiveGroupID: Darwin.getegid(),
                supplementaryGroups: supplementaryGroups
            )
        )
    }

    static func resolveAdministratorMembership(
        adminGroupID: gid_t,
        effectiveGroupID: gid_t,
        supplementaryGroups: [gid_t]
    ) -> AdministratorMembership {
        if effectiveGroupID == adminGroupID || supplementaryGroups.contains(adminGroupID) {
            return .member
        }
        return .notMember
    }
}

struct ComponentStatus: Identifiable, Equatable {
    let id: String
    let schemaVersion: Int?
    let health: ComponentHealth
    let summary: String
    let detail: String
    let updatedAt: Date?
    let incidentID: String?
    let incidentSince: Date?
    let reason: String?
    let impact: String?
    let recommendations: [String]
    let elapsedMilliseconds: Int64?
    let managedSessions: Int?
    let observationSequence: UInt64?
    let publisherInstanceID: String?
    let backendID: String?
    let mountPoint: String?
    let resourcePath: String?
    let recoveryEpochID: String?
    let recoveryEpochEstablishedAt: Date?
    let logicalBytes: Int64?
    let physicalBytes: Int64?
    let readBytesTotal: UInt64?
    let writtenBytesTotal: UInt64?
}

struct StorageMetrics: Equatable {
    let managedSessions: Int?
    let logicalBytes: Int64
    let physicalBytes: Int64

    var savedBytes: Int64 { max(0, logicalBytes - physicalBytes) }

    var savingsFraction: Double {
        guard logicalBytes > 0 else { return 0 }
        return min(1, Double(savedBytes) / Double(logicalBytes))
    }

    static func current(from components: [ComponentStatus]) -> StorageMetrics? {
        let storageComponents = components.filter {
            $0.logicalBytes.map { $0 >= 0 } == true
                && $0.physicalBytes.map { $0 >= 0 } == true
        }
        let storage = storageComponents.min { lhs, rhs in
            priority(lhs.id) < priority(rhs.id)
        }
        guard let logicalBytes = storage?.logicalBytes,
              let physicalBytes = storage?.physicalBytes else {
            return nil
        }
        let managedSessions = components.first {
            $0.id.lowercased() == "managed" && $0.managedSessions != nil
        }?.managedSessions ?? components.compactMap(\.managedSessions).max()
        return StorageMetrics(
            managedSessions: managedSessions,
            logicalBytes: logicalBytes,
            physicalBytes: physicalBytes
        )
    }

    private static func priority(_ identifier: String) -> Int {
        switch identifier.lowercased() {
        case "storage", "store": return 0
        case "managed": return 1
        default: return 2
        }
    }
}

struct IOActivity: Equatable {
    let capturedAt: Date
    let readBytesPerSecond: Double
    let writtenBytesPerSecond: Double
}

private struct IOActivityObservation: Equatable {
    let capturedAt: Date
    let publisherInstanceID: String
    let readBytesTotal: UInt64
    let writtenBytesTotal: UInt64
}

struct StorageHistorySample: Codable, Equatable, Identifiable {
    let capturedAt: Date
    let health: ComponentHealth
    let managedSessions: Int?
    let logicalBytes: Int64
    let physicalBytes: Int64
    let readBytesPerSecond: Double?
    let writtenBytesPerSecond: Double?

    var id: TimeInterval { capturedAt.timeIntervalSince1970 }
    var savedBytes: Int64 { max(0, logicalBytes - physicalBytes) }

    var savingsFraction: Double {
        guard logicalBytes > 0 else { return 0 }
        return min(1, Double(savedBytes) / Double(logicalBytes))
    }
}

struct IncidentHistoryEntry: Codable, Equatable, Identifiable {
    let occurrenceID: String
    let since: Date
    var recoveredAt: Date?

    var id: String {
        "\(occurrenceID):\(String(format: "%.6f", since.timeIntervalSince1970))"
    }

    func duration(relativeTo now: Date) -> TimeInterval {
        max(0, (recoveredAt ?? now).timeIntervalSince(since))
    }
}

struct StatusHistoryArchive: Codable, Equatable {
    static let retention: TimeInterval = 30 * 24 * 60 * 60

    private(set) var storageSamples: [StorageHistorySample] = []
    private(set) var incidents: [IncidentHistoryEntry] = []

    mutating func recordStorage(
        _ metrics: StorageMetrics?,
        activity: IOActivity? = nil,
        health: ComponentHealth,
        at capturedAt: Date
    ) -> Bool {
        guard let metrics else { return false }
        let sample = StorageHistorySample(
            capturedAt: capturedAt,
            health: health,
            managedSessions: metrics.managedSessions,
            logicalBytes: metrics.logicalBytes,
            physicalBytes: metrics.physicalBytes,
            readBytesPerSecond: activity?.readBytesPerSecond,
            writtenBytesPerSecond: activity?.writtenBytesPerSecond
        )
        let minute = Int64(floor(capturedAt.timeIntervalSince1970 / 60))
        if let index = storageSamples.lastIndex(where: {
            Int64(floor($0.capturedAt.timeIntervalSince1970 / 60)) == minute
        }) {
            storageSamples[index] = sample
            return false
        }
        storageSamples.append(sample)
        compact(referenceDate: capturedAt)
        return true
    }

    mutating func recordIncident(_ update: IncidentUpdate, at now: Date) -> Bool {
        let incident: FrontendIncident
        switch update {
        case .active(let value, _), .recovered(let value): incident = value
        }
        let existing = incidents.lastIndex {
            $0.occurrenceID == incident.id && $0.since == incident.since
        }
        if let existing {
            guard incidents[existing].recoveredAt != incident.recoveredAt else { return false }
            incidents[existing].recoveredAt = incident.recoveredAt
        } else {
            incidents.append(IncidentHistoryEntry(
                occurrenceID: incident.id,
                since: incident.since,
                recoveredAt: incident.recoveredAt
            ))
        }
        let cutoff = now.addingTimeInterval(-Self.retention)
        incidents = incidents
            .filter { ($0.recoveredAt ?? $0.since) >= cutoff }
            .sorted { $0.since > $1.since }
        if incidents.count > 200 {
            incidents.removeSubrange(200...)
        }
        return true
    }

    private mutating func compact(referenceDate: Date) {
        let cutoff = referenceDate.addingTimeInterval(-Self.retention)
        let ordered = storageSamples
            .filter { $0.capturedAt >= cutoff && $0.capturedAt <= referenceDate.addingTimeInterval(60) }
            .sorted { $0.capturedAt < $1.capturedAt }
        var latestByBucket: [String: StorageHistorySample] = [:]
        for sample in ordered {
            let age = max(0, referenceDate.timeIntervalSince(sample.capturedAt))
            let resolution: TimeInterval
            switch age {
            case 0...(6 * 60 * 60): resolution = 60
            case 0...(7 * 24 * 60 * 60): resolution = 15 * 60
            default: resolution = 60 * 60
            }
            let bucket = Int64(floor(sample.capturedAt.timeIntervalSince1970 / resolution))
            latestByBucket["\(Int(resolution)):\(bucket)"] = sample
        }
        storageSamples = latestByBucket.values.sorted { $0.capturedAt < $1.capturedAt }
    }
}

private enum StatusHistoryPersistence {
    private static let maximumBytes = 4 * 1_048_576

    static func load(appGroupURL: URL?) -> StatusHistoryArchive {
        guard let url = fileURL(appGroupURL),
              let data = try? DurableAppGroupFile.read(url, maximumBytes: maximumBytes) else {
            return StatusHistoryArchive()
        }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .millisecondsSince1970
        return (try? decoder.decode(StatusHistoryArchive.self, from: data)) ?? StatusHistoryArchive()
    }

    static func save(_ archive: StatusHistoryArchive, appGroupURL: URL?) throws {
        guard let url = fileURL(appGroupURL) else { return }
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .millisecondsSince1970
        encoder.outputFormatting = [.sortedKeys]
        try DurableAppGroupFile.write(try encoder.encode(archive), to: url)
    }

    private static func fileURL(_ appGroupURL: URL?) -> URL? {
        appGroupURL?.appendingPathComponent("ui-history-v1.json", isDirectory: false)
    }
}

private struct BackendIdentity: Equatable {
    let backendID: String?
    let mountPoint: String
    let resourcePath: String

    func matches(_ candidate: BackendIdentity?) -> Bool {
        guard let candidate else { return false }
        if let backendID {
            return candidate.backendID == backendID
                && candidate.mountPoint == mountPoint
                && candidate.resourcePath == resourcePath
        }
        return candidate.mountPoint == mountPoint && candidate.resourcePath == resourcePath
    }
}

private struct PublisherEvidence: Equatable {
    let publisherInstanceID: String
    let sequence: UInt64
}

private struct StatusContinuityAuthority: Equatable {
    let component: String
    let recoveryEpochID: String
    let recoveryEpochEstablishedAt: Date
    let continuitySequence: UInt64
    let publisherEvidence: PublisherEvidence
    let backendIdentity: BackendIdentity

    func occurrenceID(source: String) -> String {
        hashedStatusOccurrenceID(source: source, fields: [
            component,
            backendIdentity.backendID ?? "",
            backendIdentity.mountPoint,
            backendIdentity.resourcePath,
            recoveryEpochID,
        ])
    }

    func matches(_ status: ComponentStatus, requireExactHealthyPublication: Bool) -> Bool {
        guard status.recoveryEpochID == recoveryEpochID,
              backendIdentity.matches(status.backendIdentity) else {
            return false
        }
        guard requireExactHealthyPublication else { return true }
        return status.publisherEvidence == publisherEvidence
            && status.updatedAt == recoveryEpochEstablishedAt
    }
}

private func hashedStatusOccurrenceID(source: String, fields: [String]) -> String {
    let material = (["codexfold-status-occurrence-v1", source] + fields).joined(separator: "\u{0}")
    let digest = SHA256.hash(data: Data(material.utf8))
    return "\(source)-\(digest.map { String(format: "%02x", $0) }.joined())"
}

private func stableTimestamp(_ date: Date) -> String {
    String(format: "%.9f", date.timeIntervalSince1970)
}

private func statusChannelOccurrenceID(
    source: String,
    status: ComponentStatus?,
    continuity: StatusContinuityReadResult
) -> String {
    if let authority = continuity.authority {
        return authority.occurrenceID(source: source)
    }
    if let recoveryEpochID = status?.recoveryEpochID,
       let backendIdentity = status?.backendIdentity {
        return hashedStatusOccurrenceID(
            source: source,
            fields: [
                status?.id.lowercased() ?? "",
                backendIdentity.backendID ?? "",
                backendIdentity.mountPoint,
                backendIdentity.resourcePath,
                recoveryEpochID,
            ]
        )
    }
    if let incidentID = status?.incidentID,
       let incidentSince = status?.incidentSince {
        return hashedStatusOccurrenceID(
            source: source,
            fields: [incidentID, stableTimestamp(incidentSince)]
        )
    }
    return "\(source)-unverified-\(UUID().uuidString.lowercased())"
}

private func statusChannelOccurrenceSince(
    status: ComponentStatus?,
    continuity: StatusContinuityReadResult,
    at now: Date
) -> Date {
    min(
        status?.incidentSince
            ?? continuity.authority?.recoveryEpochEstablishedAt
            ?? status?.updatedAt
            ?? now,
        now
    )
}

private func statusChannelHasTrustedHealthyPublication(
    _ status: ComponentStatus,
    continuity: StatusContinuityReadResult
) -> Bool {
    guard status.recoveryEpochID != nil else { return true }
    guard let authority = continuity.authority else { return false }
    return authority.matches(status, requireExactHealthyPublication: true)
}

private enum StatusContinuityReadResult: Equatable {
    case verified(StatusContinuityAuthority)
    case unavailable

    var authority: StatusContinuityAuthority? {
        guard case .verified(let authority) = self else { return nil }
        return authority
    }
}

private enum StatusChannelReadFailureKind {
    case missing
    case invalid
}

private struct StatusChannelReadFailure {
    let kind: StatusChannelReadFailureKind
}

private enum StatusChannelReadResult {
    case valid(ComponentStatus)
    case unavailable(StatusChannelReadFailure)

    var status: ComponentStatus? {
        guard case .valid(let status) = self else { return nil }
        return status
    }
}

private extension ComponentStatus {
    var backendIdentity: BackendIdentity? {
        guard let mountPoint = mountPoint?.nonEmpty,
              let resourcePath = resourcePath?.nonEmpty else {
            return nil
        }
        return BackendIdentity(
            backendID: backendID?.nonEmpty,
            mountPoint: mountPoint,
            resourcePath: resourcePath
        )
    }

    var publisherEvidence: PublisherEvidence? {
        guard let publisherInstanceID = publisherInstanceID?.nonEmpty,
              let observationSequence,
              observationSequence > 0 else {
            return nil
        }
        return PublisherEvidence(
            publisherInstanceID: publisherInstanceID,
            sequence: observationSequence
        )
    }
}

struct FrontendIncident: Equatable {
    let id: String
    let since: Date
    let reason: String
    let impact: String
    let recommendations: [String]
    let technicalDetails: String
    var recoveredAt: Date?
}

enum IncidentUpdate: Equatable {
    case active(FrontendIncident, isNew: Bool)
    case recovered(FrontendIncident)
}

/// Tracks incident causality independently from the menu-bar lifecycle. Missing,
/// malformed, unknown, or stale observations never count as recovery evidence.
struct IncidentTracker {
    private enum Source: Hashable {
        case frontend
        case daemonStatusChannel
        case managed
        case managedStatusChannel
        case supervisorStatusChannel
        case supervisor
    }

    private struct CriticalObservation {
        var since: Date
        var identity: String
        var evidenceUpdatedAt: Date
        var evidenceSequence: UInt64?
        var evidencePublisherInstanceID: String?
        var status: ComponentStatus
    }

    private struct ActiveState {
        var incident: FrontendIncident
        var source: Source
        var upstreamIdentity: String
        var expectedIncidentID: String?
        var evidenceUpdatedAt: Date
        var evidenceSequence: UInt64?
        var evidencePublisherInstanceID: String?
        var backendIdentity: BackendIdentity?
        var requiresSupervisorHealthy: Bool
    }

    let timeout: TimeInterval
    let supervisorFreshness: TimeInterval

    private var incidents: [Source: ActiveState] = [:]
    private var selectedSource: Source?
    private var criticalObservations: [Source: CriticalObservation] = [:]
    private var supervisorObservedSince: Date?
    private var supervisorObservedIncidentID: String?
    private var supervisorLastReliableAt: Date?

    init(timeout: TimeInterval = 10, supervisorFreshness: TimeInterval = 8) {
        self.timeout = max(0, timeout)
        self.supervisorFreshness = max(1, supervisorFreshness)
    }

    var currentIncident: FrontendIncident? {
        guard let selectedSource else { return nil }
        return incidents[selectedSource]?.incident
    }

    mutating func evaluate(
        frontend: ComponentStatus?,
        managed: ComponentStatus? = nil,
        daemonStatusChannel: ComponentStatus? = nil,
        managedStatusChannel: ComponentStatus? = nil,
        supervisorStatusChannel: ComponentStatus? = nil,
        supervisor: ComponentStatus?,
        at now: Date
    ) -> IncidentUpdate? {
        let previousSource = selectedSource
        let previousState = previousSource.flatMap { incidents[$0] }

        updateCriticalIncident(source: .frontend, status: frontend, at: now)
        updateCriticalIncident(source: .daemonStatusChannel, status: daemonStatusChannel, at: now)
        updateCriticalIncident(source: .managed, status: managed, at: now)
        updateCriticalIncident(source: .managedStatusChannel, status: managedStatusChannel, at: now)
        updateCriticalIncident(source: .supervisorStatusChannel, status: supervisorStatusChannel, at: now)
        updateSupervisorIncident(supervisor, frontend: frontend, at: now)

        resolveCriticalIncident(source: .frontend, status: frontend, supervisor: supervisor, at: now)
        resolveCriticalIncident(
            source: .daemonStatusChannel,
            status: daemonStatusChannel,
            supervisor: supervisor,
            at: now
        )
        resolveCriticalIncident(source: .managed, status: managed, supervisor: supervisor, at: now)
        resolveCriticalIncident(
            source: .managedStatusChannel,
            status: managedStatusChannel,
            supervisor: supervisor,
            at: now
        )
        resolveCriticalIncident(
            source: .supervisorStatusChannel,
            status: supervisorStatusChannel,
            supervisor: supervisor,
            at: now
        )
        resolveSupervisorIncident(supervisor: supervisor, frontend: frontend, at: now)

        if previousSource == .supervisor,
           let supervisorState = incidents[.supervisor],
           let frontendState = incidents[.frontend] {
            let merged = mergeSupervisorIncident(supervisorState, with: frontendState)
            incidents[.supervisor] = nil
            incidents[.frontend] = merged
            selectedSource = .frontend
            return merged.incident == previousState?.incident
                ? nil
                : .active(merged.incident, isNew: false)
        }

        if let previousSource, let current = incidents[previousSource] {
            selectedSource = previousSource
            let identityChanged = current.upstreamIdentity != previousState?.upstreamIdentity
            if identityChanged || current.incident != previousState?.incident {
                return .active(current.incident, isNew: identityChanged)
            }
            return nil
        }

        if let next = nextIncident() {
            selectedSource = next.source
            return .active(
                next.incident,
                isNew: next.incident.id != previousState?.incident.id
            )
        }

        selectedSource = nil
        guard var recovered = previousState?.incident else { return nil }
        recovered.recoveredAt = now
        return .recovered(recovered)
    }

    private mutating func updateCriticalIncident(
        source: Source,
        status: ComponentStatus?,
        at now: Date
    ) {
        guard source == .frontend
                || source == .daemonStatusChannel
                || source == .managed
                || source == .managedStatusChannel
                || source == .supervisorStatusChannel else {
            return
        }
        guard let status else {
            materializePreservedManagedIncidentIfNeeded(source: source, at: now)
            return
        }
        guard Self.isCriticalFailure(status) else {
            if status.health == .healthy,
               let observation = criticalObservations[source],
               isRecoveryEvidence(
                   source: source,
                   status: status,
                   expectedIncidentID: observation.status.incidentID,
                   incidentSince: observation.since,
                   evidenceUpdatedAt: observation.evidenceUpdatedAt,
                   evidenceSequence: observation.evidenceSequence,
                   evidencePublisherInstanceID: observation.evidencePublisherInstanceID,
                   backendIdentity: observation.status.backendIdentity,
                   at: now
               ) {
                criticalObservations[source] = nil
            }
            return
        }

        let observation = observeCriticalFailure(source: source, status: status, at: now)
        guard incidents[source] != nil
                || now.timeIntervalSince(observation.since) >= timeout else {
            return
        }

        var candidate = criticalState(
            source: source,
            status: observation.status,
            observation: observation
        )
        if incidents[source]?.requiresSupervisorHealthy == true {
            candidate.requiresSupervisorHealthy = true
        }
        incidents[source] = candidate
    }

    private mutating func materializePreservedManagedIncidentIfNeeded(source: Source, at now: Date) {
        guard source == .managed,
              incidents[source] == nil,
              let observation = criticalObservations[source],
              now.timeIntervalSince(observation.since) >= timeout else {
            return
        }
        incidents[source] = criticalState(
            source: source,
            status: observation.status,
            observation: observation
        )
    }

    private mutating func observeCriticalFailure(
        source: Source,
        status: ComponentStatus,
        at now: Date
    ) -> CriticalObservation {
        let existing = criticalObservations[source]
        let expectedIdentity = status.incidentID.map {
            Self.identity(source: source, incidentID: $0, since: now)
        }
        let matchingSince: Date? = {
            guard let existing else { return nil }
            if let expectedIdentity {
                return existing.identity == expectedIdentity ? existing.since : nil
            }
            return existing.identity.hasPrefix("\(Self.sourceName(source))-legacy:")
                ? existing.since
                : nil
        }()
        let declaredSince = status.incidentSince
            ?? matchingSince
            ?? status.elapsedMilliseconds.map {
                now.addingTimeInterval(-Double(max(0, $0)) / 1_000)
            }
            ?? now
        let identity = Self.identity(source: source, incidentID: status.incidentID, since: declaredSince)
        let evidenceUpdatedAt = status.updatedAt ?? now
        let evidenceSequence = status.observationSequence
        let evidencePublisherInstanceID = status.publisherEvidence?.publisherInstanceID

        let observation: CriticalObservation
        if let existing, existing.identity == identity {
            let useNewStatus = Self.isCriticalEvidenceNewer(
                source: source,
                status: status,
                updatedAt: evidenceUpdatedAt,
                existing: existing
            )
            observation = CriticalObservation(
                since: min(existing.since, declaredSince),
                identity: identity,
                evidenceUpdatedAt: useNewStatus ? evidenceUpdatedAt : existing.evidenceUpdatedAt,
                evidenceSequence: useNewStatus ? evidenceSequence : existing.evidenceSequence,
                evidencePublisherInstanceID: useNewStatus
                    ? evidencePublisherInstanceID
                    : existing.evidencePublisherInstanceID,
                status: useNewStatus ? status : existing.status
            )
        } else {
            observation = CriticalObservation(
                since: declaredSince,
                identity: identity,
                evidenceUpdatedAt: evidenceUpdatedAt,
                evidenceSequence: evidenceSequence,
                evidencePublisherInstanceID: evidencePublisherInstanceID,
                status: status
            )
        }
        criticalObservations[source] = observation
        return observation
    }

    private mutating func updateSupervisorIncident(
        _ supervisor: ComponentStatus?,
        frontend: ComponentStatus?,
        at now: Date
    ) {
        guard let supervisor else { return }
        if supervisor.health == .healthy, isReliableSupervisor(supervisor, at: now) {
            resetSupervisorObservation()
            return
        }
        guard supervisor.health == .recovering,
              isReliableSupervisor(supervisor, at: now) else {
            // Preserve the observation over a transient read failure. A later fresh
            // recovery sample proves continuity; a long observation gap restarts it.
            return
        }
        guard frontendIsObsolete(frontend, relativeTo: supervisor) else {
            // The complete liveness condition is no longer true. A later supervisor
            // update may make this frontend record obsolete and begin a new interval.
            resetSupervisorObservation()
            return
        }

        let declaredSince = supervisor.incidentSince
            ?? supervisor.elapsedMilliseconds.map {
                now.addingTimeInterval(-Double(max(0, $0)) / 1_000)
            }
            ?? now
        if supervisorObservedIncidentID != supervisor.incidentID {
            supervisorObservedIncidentID = supervisor.incidentID
            supervisorObservedSince = declaredSince
        } else if let lastReliableAt = supervisorLastReliableAt,
                  now.timeIntervalSince(lastReliableAt) > supervisorFreshness {
            supervisorObservedSince = supervisor.incidentID == nil ? now : declaredSince
        } else if let observedSince = supervisorObservedSince {
            supervisorObservedSince = min(observedSince, declaredSince)
        } else {
            supervisorObservedSince = declaredSince
        }
        supervisorLastReliableAt = now
        guard now.timeIntervalSince(supervisorObservedSince ?? now) >= timeout else { return }

        let since = supervisorObservedSince ?? now
        let identity = supervisor.recoveryEpochID == nil
            ? supervisor.incidentID ?? "supervisor:\(Int64(since.timeIntervalSince1970 * 1_000))"
            : statusChannelOccurrenceID(
                source: "supervisor-status-channel",
                status: supervisor,
                continuity: .unavailable
            )
        let incident = FrontendIncident(
            id: identity,
            since: since,
            reason: L10n.text(.supervisorRecoveryReason),
            impact: L10n.text(.supervisorFailureImpact),
            recommendations: [L10n.text(.retryRecommendation), L10n.text(.openDashboardRecommendation)],
            technicalDetails: supervisor.detail.nonEmpty ?? supervisor.summary,
            recoveredAt: nil
        )
        incidents[.supervisor] = ActiveState(
            incident: incident,
            source: .supervisor,
            upstreamIdentity: identity,
            expectedIncidentID: supervisor.incidentID,
            // The incident begins at the first reliable observation. A frontend
            // healthy record published later in that interval remains valid even
            // if the UI reads it after the ten-second alert is emitted.
            evidenceUpdatedAt: since,
            evidenceSequence: supervisor.observationSequence,
            evidencePublisherInstanceID: nil,
            backendIdentity: supervisor.backendIdentity,
            requiresSupervisorHealthy: true
        )
    }

    private mutating func resolveCriticalIncident(
        source: Source,
        status: ComponentStatus?,
        supervisor: ComponentStatus?,
        at now: Date
    ) {
        guard let current = incidents[source],
              let status,
              status.health == .healthy,
              isRecoveryEvidence(
                  source: source,
                  status: status,
                  expectedIncidentID: current.expectedIncidentID,
                  incidentSince: current.incident.since,
                  evidenceUpdatedAt: current.evidenceUpdatedAt,
                  evidenceSequence: current.evidenceSequence,
                  evidencePublisherInstanceID: current.evidencePublisherInstanceID,
                  backendIdentity: current.backendIdentity,
                  at: now
              ) else {
            return
        }
        if current.requiresSupervisorHealthy {
            guard let supervisor,
                  supervisor.health == .healthy,
                  let supervisorUpdatedAt = supervisor.updatedAt,
                  supervisorUpdatedAt >= current.evidenceUpdatedAt,
                  supervisorUpdatedAt <= now.addingTimeInterval(60) else {
                return
            }
        }
        incidents[source] = nil
    }

    private func isRecoveryEvidence(
        source: Source,
        status: ComponentStatus,
        expectedIncidentID: String?,
        incidentSince: Date,
        evidenceUpdatedAt: Date,
        evidenceSequence: UInt64?,
        evidencePublisherInstanceID: String?,
        backendIdentity: BackendIdentity?,
        at now: Date
    ) -> Bool {
        guard status.health == .healthy,
              let updatedAt = status.updatedAt,
              updatedAt <= now.addingTimeInterval(60) else {
            return false
        }

        let publisherOrdered = source == .managed
            || source == .daemonStatusChannel
            || source == .managedStatusChannel
            || source == .supervisorStatusChannel
        let matchingManagedBackend = (source != .managed
            && source != .daemonStatusChannel
            && source != .supervisorStatusChannel)
            || backendIdentity == nil
            || backendIdentity?.matches(status.backendIdentity) == true
        guard matchingManagedBackend else { return false }

        if publisherOrdered, let evidencePublisherInstanceID {
            guard let recovery = status.publisherEvidence,
                  let evidenceSequence else {
                // Once an incident carries schema-v2 publisher evidence, an
                // incomplete or legacy status cannot downgrade causality to a
                // wall-clock comparison.
                return false
            }
            if recovery.publisherInstanceID == evidencePublisherInstanceID {
                return recovery.sequence > evidenceSequence
            }
            return isFreshPublisherRecovery(status, at: now)
        }

        if publisherOrdered, status.publisherEvidence != nil {
            // A fresh schema-v2 publisher is authoritative for a legacy
            // incident only when it is the same logical managed backend.
            return isFreshPublisherRecovery(status, at: now)
        }

        let newer = Self.isEvidenceNewer(
            updatedAt: updatedAt,
            sequence: status.observationSequence,
            thanUpdatedAt: evidenceUpdatedAt,
            sequence: evidenceSequence
        )
        if let expectedIncidentID,
           status.incidentID == expectedIncidentID,
           newer,
           Self.isCausallyNewerDate(
               updatedAt,
               than: incidentSince,
               sequence: status.observationSequence,
               previousSequence: evidenceSequence
           ) {
            return true
        }

        guard source == .managed,
              let backendIdentity,
              backendIdentity.matches(status.backendIdentity),
              let evidenceSequence,
              let recoverySequence = status.observationSequence else {
            return false
        }
        return recoverySequence > evidenceSequence
    }

    private func isFreshPublisherRecovery(_ status: ComponentStatus, at now: Date) -> Bool {
        guard let updatedAt = status.updatedAt,
              updatedAt <= now.addingTimeInterval(2) else {
            return false
        }
        return now.timeIntervalSince(updatedAt) <= max(timeout, 1)
    }

    private mutating func resolveSupervisorIncident(
        supervisor: ComponentStatus?,
        frontend: ComponentStatus?,
        at now: Date
    ) {
        guard let current = incidents[.supervisor],
              let frontend,
              frontend.health == .healthy,
              let frontendUpdatedAt = frontend.updatedAt,
              frontendUpdatedAt > current.evidenceUpdatedAt,
              frontendUpdatedAt >= current.incident.since,
              frontendUpdatedAt <= now.addingTimeInterval(60),
              let supervisor,
              supervisor.health == .healthy,
              let supervisorUpdatedAt = supervisor.updatedAt,
              supervisorUpdatedAt >= current.evidenceUpdatedAt,
              supervisorUpdatedAt <= now.addingTimeInterval(60) else {
            return
        }
        incidents[.supervisor] = nil
    }

    private func criticalState(
        source: Source,
        status: ComponentStatus,
        observation: CriticalObservation
    ) -> ActiveState {
        let incidentID: String
        if source == .managed, status.recoveryEpochID != nil {
            incidentID = statusChannelOccurrenceID(
                source: "managed-status-channel",
                status: status,
                continuity: .unavailable
            )
        } else {
            incidentID = status.incidentID ?? observation.identity
        }
        let reason: String
        let impact: String
        let recommendations: [String]
        switch source {
        case .frontend:
            reason = L10n.text(.unknownReason)
            impact = L10n.text(.unknownImpact)
            recommendations = [L10n.text(.retryRecommendation), L10n.text(.openDashboardRecommendation)]
        case .managed:
            reason = L10n.text(.managedStatusChannelReason)
            impact = L10n.text(.managedStatusChannelImpact)
            recommendations = [
                L10n.text(.managedStatusChannelWaitRecommendation),
                L10n.text(.openDashboardRecommendation),
            ]
        case .daemonStatusChannel, .managedStatusChannel, .supervisorStatusChannel, .supervisor:
            reason = status.reason ?? status.summary.nonEmpty ?? L10n.text(.unknownReason)
            impact = status.impact ?? L10n.text(.unknownImpact)
            recommendations = status.recommendations.isEmpty
                ? [L10n.text(.retryRecommendation), L10n.text(.openDashboardRecommendation)]
                : status.recommendations
        }
        let incident = FrontendIncident(
            id: incidentID,
            since: observation.since,
            reason: reason,
            impact: impact,
            recommendations: recommendations,
            technicalDetails: status.detail.nonEmpty ?? status.summary,
            recoveredAt: nil
        )
        return ActiveState(
            incident: incident,
            source: source,
            upstreamIdentity: observation.identity,
            expectedIncidentID: status.incidentID,
            evidenceUpdatedAt: observation.evidenceUpdatedAt,
            evidenceSequence: observation.evidenceSequence,
            evidencePublisherInstanceID: observation.evidencePublisherInstanceID,
            backendIdentity: status.backendIdentity,
            requiresSupervisorHealthy: false
        )
    }

    private func mergeSupervisorIncident(
        _ supervisorState: ActiveState,
        with frontendState: ActiveState
    ) -> ActiveState {
        let incident = FrontendIncident(
            id: supervisorState.incident.id,
            since: min(supervisorState.incident.since, frontendState.incident.since),
            reason: frontendState.incident.reason,
            impact: frontendState.incident.impact,
            recommendations: frontendState.incident.recommendations,
            technicalDetails: frontendState.incident.technicalDetails,
            recoveredAt: nil
        )
        return ActiveState(
            incident: incident,
            source: .frontend,
            upstreamIdentity: frontendState.upstreamIdentity,
            expectedIncidentID: frontendState.expectedIncidentID,
            evidenceUpdatedAt: max(supervisorState.evidenceUpdatedAt, frontendState.evidenceUpdatedAt),
            evidenceSequence: Self.maximumSequence(
                supervisorState.evidenceSequence,
                frontendState.evidenceSequence
            ),
            evidencePublisherInstanceID: frontendState.evidencePublisherInstanceID,
            backendIdentity: frontendState.backendIdentity,
            requiresSupervisorHealthy: true
        )
    }

    private func nextIncident() -> ActiveState? {
        for source in [Source.frontend, .daemonStatusChannel, .managed, .managedStatusChannel, .supervisorStatusChannel, .supervisor] {
            if let incident = incidents[source] { return incident }
        }
        return nil
    }

    private mutating func resetSupervisorObservation() {
        supervisorObservedSince = nil
        supervisorObservedIncidentID = nil
        supervisorLastReliableAt = nil
    }

    private func isReliableSupervisor(_ supervisor: ComponentStatus, at now: Date) -> Bool {
        guard let updatedAt = supervisor.updatedAt,
              updatedAt <= now.addingTimeInterval(2) else {
            return false
        }
        return now.timeIntervalSince(updatedAt) <= supervisorFreshness
    }

    private func frontendIsObsolete(
        _ frontend: ComponentStatus?,
        relativeTo supervisor: ComponentStatus
    ) -> Bool {
        guard let frontend, frontend.health == .healthy else { return true }
        guard let frontendUpdatedAt = frontend.updatedAt,
              let supervisorUpdatedAt = supervisor.updatedAt else {
            return true
        }
        return frontendUpdatedAt < supervisorUpdatedAt
    }

    private static func isCriticalFailure(_ status: ComponentStatus) -> Bool {
        status.health == .recovering || status.health == .failed
    }

    private static func identity(source: Source, incidentID: String?, since: Date) -> String {
        if let incidentID {
            return "\(sourceName(source)):\(incidentID)"
        }
        return "\(sourceName(source))-legacy:\(Int64(since.timeIntervalSince1970 * 1_000))"
    }

    private static func sourceName(_ source: Source) -> String {
        switch source {
        case .frontend: return "frontend"
        case .daemonStatusChannel: return "daemon-status-channel"
        case .managed: return "managed"
        case .managedStatusChannel: return "managed-status-channel"
        case .supervisorStatusChannel: return "supervisor-status-channel"
        case .supervisor: return "supervisor"
        }
    }

    private static func isCriticalEvidenceNewer(
        source: Source,
        status: ComponentStatus,
        updatedAt: Date,
        existing: CriticalObservation
    ) -> Bool {
        if source == .managed
            || source == .daemonStatusChannel
            || source == .managedStatusChannel
            || source == .supervisorStatusChannel {
            let candidate = status.publisherEvidence
            if let existingPublisherInstanceID = existing.evidencePublisherInstanceID {
                guard let candidate, let existingSequence = existing.evidenceSequence else {
                    return false
                }
                if candidate.publisherInstanceID == existingPublisherInstanceID {
                    return candidate.sequence > existingSequence
                }
                return true
            }
            if candidate != nil {
                return true
            }
        }
        return isEvidenceNewer(
            updatedAt: updatedAt,
            sequence: status.observationSequence,
            thanUpdatedAt: existing.evidenceUpdatedAt,
            sequence: existing.evidenceSequence
        )
    }

    private static func isEvidenceNewer(
        updatedAt: Date,
        sequence: UInt64?,
        thanUpdatedAt previousUpdatedAt: Date,
        sequence previousSequence: UInt64?
    ) -> Bool {
        if let sequence, let previousSequence {
            return sequence > previousSequence
        }
        return updatedAt > previousUpdatedAt
    }

    private static func isCausallyNewerDate(
        _ updatedAt: Date,
        than incidentSince: Date,
        sequence: UInt64?,
        previousSequence: UInt64?
    ) -> Bool {
        if let sequence, let previousSequence, sequence > previousSequence {
            return true
        }
        return updatedAt >= incidentSince
    }

    private static func maximumSequence(_ lhs: UInt64?, _ rhs: UInt64?) -> UInt64? {
        switch (lhs, rhs) {
        case let (lhs?, rhs?): return max(lhs, rhs)
        case let (lhs?, nil): return lhs
        case let (nil, rhs?): return rhs
        case (nil, nil): return nil
        }
    }
}

private struct DaemonStatusChannelMonitor {
    private struct Outage {
        let id: String
        let since: Date
        var minimumPublisherInstanceID: String?
        var minimumSequence: UInt64?
        var minimumUpdatedAt: Date?
        var backendIdentity: BackendIdentity?
        var detail: String
    }

    private let freshness: TimeInterval
    private let futureTolerance: TimeInterval
    private var outage: Outage?
    private var lastProgressPublisherInstanceID: String?
    private var lastProgressSequence: UInt64?
    private var lastProgressUpdatedAt: Date?
    private var lastProgressObservedAt: Date?
    private var lastBackendIdentity: BackendIdentity?

    init(freshness: TimeInterval = 10, futureTolerance: TimeInterval = 2) {
        self.freshness = max(1, freshness)
        self.futureTolerance = max(0, futureTolerance)
    }

    mutating func evaluate(
        _ result: StatusChannelReadResult,
        continuity: StatusContinuityReadResult,
        at now: Date
    ) -> ComponentStatus? {
        guard case .valid(let status) = result, isStructurallyTrustworthy(status) else {
            let failureKind: StatusChannelReadFailureKind
            if case .unavailable(let failure) = result {
                failureKind = failure.kind
            } else {
                failureKind = .invalid
            }
            let authority = continuity.authority
            let baseDetail = failureKind == .missing
                ? L10n.text(.daemonStatusChannelMissingDetail)
                : L10n.text(.daemonStatusChannelInvalidDetail)
            let current = ensureOutage(
                id: authority?.occurrenceID(source: "daemon-status-channel")
                    ?? statusChannelOccurrenceID(
                        source: "daemon-status-channel",
                        status: nil,
                        continuity: continuity
                    ),
                since: authority.map { min($0.recoveryEpochEstablishedAt, now) } ?? now,
                evidence: nil,
                minimumUpdatedAt: nil,
                backendIdentity: lastBackendIdentity,
                detail: authority == nil
                    ? "\(baseDetail)\n\(L10n.text(.daemonStatusChannelContinuityInvalidDetail))"
                    : baseDetail
            )
            return failureStatus(for: current, at: now)
        }
        guard isFresh(status, at: now) else {
            let current = ensureOutage(
                id: statusChannelOccurrenceID(
                    source: "daemon-status-channel",
                    status: status,
                    continuity: continuity
                ),
                since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
                evidence: status.publisherEvidence,
                minimumUpdatedAt: status.updatedAt,
                backendIdentity: status.backendIdentity,
                detail: L10n.text(.daemonStatusChannelStaleDetail)
            )
            return failureStatus(for: current, at: now)
        }

        if var current = outage {
            guard advances(status, beyond: current) else {
                return failureStatus(for: current, at: now)
            }
            if status.health == .healthy {
                guard statusChannelHasTrustedHealthyPublication(status, continuity: continuity) else {
                    current.detail = L10n.text(.daemonStatusChannelContinuityInvalidDetail)
                    outage = current
                    return failureStatus(for: current, at: now)
                }
                outage = nil
                recordProgress(status, at: now)
                return recoveryStatus(for: current, status: status)
            }
            current.minimumPublisherInstanceID = status.publisherEvidence?.publisherInstanceID
            current.minimumSequence = status.publisherEvidence?.sequence
            current.minimumUpdatedAt = status.updatedAt
            current.backendIdentity = status.backendIdentity
            current.detail = status.detail.nonEmpty ?? status.summary
            outage = current
            return failureStatus(for: current, at: now)
        }

        if status.health == .healthy {
            guard statusChannelHasTrustedHealthyPublication(status, continuity: continuity) else {
                let current = ensureOutage(
                    id: statusChannelOccurrenceID(
                        source: "daemon-status-channel",
                        status: status,
                        continuity: continuity
                    ),
                    since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
                    evidence: status.publisherEvidence,
                    minimumUpdatedAt: status.updatedAt,
                    backendIdentity: status.backendIdentity,
                    detail: L10n.text(.daemonStatusChannelContinuityInvalidDetail)
                )
                return failureStatus(for: current, at: now)
            }
            if lastProgressObservedAt == nil || hasProgressed(status) {
                recordProgress(status, at: now)
                return nil
            }
            guard now.timeIntervalSince(lastProgressObservedAt ?? now) >= freshness else {
                return nil
            }
            let current = ensureOutage(
                id: statusChannelOccurrenceID(
                    source: "daemon-status-channel",
                    status: status,
                    continuity: continuity
                ),
                since: lastProgressObservedAt ?? now,
                evidence: lastProgressEvidence,
                minimumUpdatedAt: lastProgressUpdatedAt,
                backendIdentity: lastBackendIdentity,
                detail: L10n.text(.daemonStatusChannelStaleDetail)
            )
            return failureStatus(for: current, at: now)
        }

        let current = ensureOutage(
            id: statusChannelOccurrenceID(
                source: "daemon-status-channel",
                status: status,
                continuity: continuity
            ),
            since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
            evidence: status.publisherEvidence,
            minimumUpdatedAt: status.updatedAt,
            backendIdentity: status.backendIdentity,
            detail: status.detail.nonEmpty ?? status.summary
        )
        return failureStatus(for: current, at: now)
    }

    private mutating func ensureOutage(
        id: String,
        since: Date,
        evidence: PublisherEvidence?,
        minimumUpdatedAt: Date?,
        backendIdentity: BackendIdentity?,
        detail: String
    ) -> Outage {
        if var current = outage {
            current.detail = detail
            self.outage = current
            return current
        }
        let current = Outage(
            id: id,
            since: since,
            minimumPublisherInstanceID: evidence?.publisherInstanceID ?? lastProgressPublisherInstanceID,
            minimumSequence: evidence?.sequence ?? lastProgressSequence,
            minimumUpdatedAt: minimumUpdatedAt ?? lastProgressUpdatedAt,
            backendIdentity: backendIdentity ?? lastBackendIdentity,
            detail: detail
        )
        outage = current
        return current
    }

    private func advances(_ status: ComponentStatus, beyond outage: Outage) -> Bool {
        guard let candidate = status.publisherEvidence,
              let candidateBackend = status.backendIdentity else {
            return false
        }
        if let expectedBackend = outage.backendIdentity,
           !expectedBackend.matches(candidateBackend) {
            return false
        }
        if let minimumPublisherInstanceID = outage.minimumPublisherInstanceID {
            if candidate.publisherInstanceID == minimumPublisherInstanceID {
                guard let minimumSequence = outage.minimumSequence else { return false }
                return candidate.sequence > minimumSequence
            }
            return true
        }
        guard let updatedAt = status.updatedAt else { return false }
        return updatedAt > (outage.minimumUpdatedAt ?? outage.since)
    }

    private func isStructurallyTrustworthy(_ status: ComponentStatus) -> Bool {
        guard status.schemaVersion == 2,
              status.health != .unknown,
              status.publisherEvidence != nil,
              status.backendIdentity != nil,
              let mountPoint = status.mountPoint,
              let resourcePath = status.resourcePath else {
            return false
        }
        return (mountPoint as NSString).isAbsolutePath
            && (resourcePath as NSString).isAbsolutePath
    }

    private func isFresh(_ status: ComponentStatus, at now: Date) -> Bool {
        guard let updatedAt = status.updatedAt,
              updatedAt <= now.addingTimeInterval(futureTolerance) else {
            return false
        }
        return now.timeIntervalSince(updatedAt) <= freshness
    }

    private func hasProgressed(_ status: ComponentStatus) -> Bool {
        guard let candidate = status.publisherEvidence,
              let candidateBackend = status.backendIdentity else {
            return false
        }
        if let lastBackendIdentity, !lastBackendIdentity.matches(candidateBackend) {
            return true
        }
        guard let lastProgressPublisherInstanceID else { return true }
        if candidate.publisherInstanceID != lastProgressPublisherInstanceID {
            return true
        }
        guard let lastProgressSequence else { return true }
        return candidate.sequence > lastProgressSequence
    }

    private mutating func recordProgress(_ status: ComponentStatus, at now: Date) {
        lastProgressPublisherInstanceID = status.publisherEvidence?.publisherInstanceID
        lastProgressSequence = status.publisherEvidence?.sequence
        lastProgressUpdatedAt = status.updatedAt
        lastProgressObservedAt = now
        lastBackendIdentity = status.backendIdentity
    }

    private var lastProgressEvidence: PublisherEvidence? {
        guard let lastProgressPublisherInstanceID, let lastProgressSequence else { return nil }
        return PublisherEvidence(
            publisherInstanceID: lastProgressPublisherInstanceID,
            sequence: lastProgressSequence
        )
    }

    private func failureStatus(for outage: Outage, at now: Date) -> ComponentStatus {
        ComponentStatus(
            id: "daemon-status-channel",
            schemaVersion: nil,
            health: .recovering,
            summary: L10n.text(.daemonStatusChannelSummary),
            detail: outage.detail,
            updatedAt: outage.minimumUpdatedAt ?? outage.since,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: L10n.text(.daemonStatusChannelReason),
            impact: L10n.text(.daemonStatusChannelImpact),
            recommendations: [
                L10n.text(.daemonStatusChannelWaitRecommendation),
                L10n.text(.openDashboardRecommendation),
            ],
            elapsedMilliseconds: Int64(max(0, now.timeIntervalSince(outage.since)) * 1_000),
            managedSessions: nil,
            observationSequence: outage.minimumSequence,
            publisherInstanceID: outage.minimumPublisherInstanceID,
            backendID: outage.backendIdentity?.backendID,
            mountPoint: outage.backendIdentity?.mountPoint,
            resourcePath: outage.backendIdentity?.resourcePath,
            recoveryEpochID: nil,
            recoveryEpochEstablishedAt: nil,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }

    private func recoveryStatus(for outage: Outage, status: ComponentStatus) -> ComponentStatus {
        ComponentStatus(
            id: "daemon-status-channel",
            schemaVersion: nil,
            health: .healthy,
            summary: L10n.text(.daemonStatusChannelRecoveredSummary),
            detail: "",
            updatedAt: status.updatedAt,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: nil,
            impact: nil,
            recommendations: [],
            elapsedMilliseconds: nil,
            managedSessions: nil,
            observationSequence: status.observationSequence,
            publisherInstanceID: status.publisherInstanceID,
            backendID: status.backendID,
            mountPoint: status.mountPoint,
            resourcePath: status.resourcePath,
            recoveryEpochID: status.recoveryEpochID,
            recoveryEpochEstablishedAt: status.recoveryEpochEstablishedAt,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }
}

private struct ManagedStatusChannelMonitor {
    private struct Outage {
        let id: String
        let since: Date
        let minimumPublisherInstanceID: String?
        let minimumSequence: UInt64?
        let minimumUpdatedAt: Date?
        var detail: String
    }

    private let freshness: TimeInterval
    private let futureTolerance: TimeInterval
    private var outage: Outage?
    private var lastProgressPublisherInstanceID: String?
    private var lastProgressSequence: UInt64?
    private var lastProgressUpdatedAt: Date?
    private var lastProgressObservedAt: Date?

    init(freshness: TimeInterval = 10, futureTolerance: TimeInterval = 2) {
        self.freshness = max(1, freshness)
        self.futureTolerance = max(0, futureTolerance)
    }

    mutating func evaluate(
        _ result: StatusChannelReadResult,
        continuity: StatusContinuityReadResult,
        at now: Date
    ) -> ComponentStatus? {
        switch result {
        case .unavailable(let failure):
            let authority = continuity.authority
            let baseDetail = failure.kind == .missing
                ? L10n.text(.managedStatusChannelMissingDetail)
                : L10n.text(.managedStatusChannelInvalidDetail)
            let outage = ensureOutage(
                id: authority?.occurrenceID(source: "managed-status-channel")
                    ?? statusChannelOccurrenceID(
                        source: "managed-status-channel",
                        status: nil,
                        continuity: continuity
                    ),
                since: authority.map { min($0.recoveryEpochEstablishedAt, now) } ?? now,
                detail: authority == nil
                    ? "\(baseDetail)\n\(L10n.text(.managedStatusChannelContinuityInvalidDetail))"
                    : baseDetail
            )
            return failureStatus(for: outage, at: now)

        case .valid(let status):
            if let outage {
                guard isFresh(status, at: now), advancesBeyondOutage(status, outage: outage) else {
                    return failureStatus(for: outage, at: now)
                }
                if status.health == .healthy,
                   !statusChannelHasTrustedHealthyPublication(status, continuity: continuity) {
                    var current = outage
                    current.detail = L10n.text(.managedStatusChannelContinuityInvalidDetail)
                    self.outage = current
                    return failureStatus(for: current, at: now)
                }
                self.outage = nil
                recordProgress(status, at: now)
                return recoveryStatus(for: outage, status: status)
            }

            if status.health == .recovering || status.health == .failed {
                if hasProgressed(status) || lastProgressObservedAt == nil {
                    recordProgress(status, at: now)
                }
                return nil
            }

            guard statusChannelHasTrustedHealthyPublication(status, continuity: continuity) else {
                let outage = ensureOutage(
                    id: statusChannelOccurrenceID(
                        source: "managed-status-channel",
                        status: status,
                        continuity: continuity
                    ),
                    since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
                    detail: L10n.text(.managedStatusChannelContinuityInvalidDetail)
                )
                return failureStatus(for: outage, at: now)
            }

            if lastProgressObservedAt == nil {
                recordProgress(status, at: now)
                guard isFresh(status, at: now) else {
                    let outage = ensureOutage(
                        id: statusChannelOccurrenceID(
                            source: "managed-status-channel",
                            status: status,
                            continuity: continuity
                        ),
                        since: now,
                        detail: L10n.text(.managedStatusChannelStaleDetail)
                    )
                    return failureStatus(for: outage, at: now)
                }
                return nil
            }

            if hasProgressed(status), isFresh(status, at: now) {
                recordProgress(status, at: now)
                return nil
            }

            let outage = ensureOutage(
                id: statusChannelOccurrenceID(
                    source: "managed-status-channel",
                    status: status,
                    continuity: continuity
                ),
                since: lastProgressObservedAt ?? now,
                detail: L10n.text(.managedStatusChannelStaleDetail)
            )
            return failureStatus(for: outage, at: now)
        }
    }

    private mutating func ensureOutage(id: String, since: Date, detail: String) -> Outage {
        if var outage {
            outage.detail = detail
            self.outage = outage
            return outage
        }
        let outage = Outage(
            id: id,
            since: since,
            minimumPublisherInstanceID: lastProgressPublisherInstanceID,
            minimumSequence: lastProgressSequence,
            minimumUpdatedAt: lastProgressUpdatedAt,
            detail: detail
        )
        self.outage = outage
        return outage
    }

    private func isFresh(_ status: ComponentStatus, at now: Date) -> Bool {
        guard let updatedAt = status.updatedAt,
              updatedAt <= now.addingTimeInterval(futureTolerance) else {
            return false
        }
        return now.timeIntervalSince(updatedAt) <= freshness
    }

    private func hasProgressed(_ status: ComponentStatus) -> Bool {
        if let candidate = status.publisherEvidence {
            guard let lastProgressPublisherInstanceID else { return true }
            if candidate.publisherInstanceID != lastProgressPublisherInstanceID {
                return true
            }
            guard let lastProgressSequence else { return true }
            return candidate.sequence > lastProgressSequence
        }
        if lastProgressPublisherInstanceID != nil {
            return false
        }
        if let sequence = status.observationSequence {
            guard let lastProgressSequence else { return true }
            return sequence > lastProgressSequence
        }
        guard lastProgressSequence == nil,
              let updatedAt = status.updatedAt else {
            return false
        }
        return updatedAt > (lastProgressUpdatedAt ?? .distantPast)
    }

    private func advancesBeyondOutage(_ status: ComponentStatus, outage: Outage) -> Bool {
        if let minimumPublisherInstanceID = outage.minimumPublisherInstanceID {
            guard let candidate = status.publisherEvidence else { return false }
            if candidate.publisherInstanceID != minimumPublisherInstanceID {
                return true
            }
            guard let minimumSequence = outage.minimumSequence else { return true }
            return candidate.sequence > minimumSequence
        }
        if status.publisherEvidence != nil {
            return true
        }
        if let minimumSequence = outage.minimumSequence {
            guard let sequence = status.observationSequence else { return false }
            return sequence > minimumSequence
        }
        guard let updatedAt = status.updatedAt else { return false }
        if let minimumUpdatedAt = outage.minimumUpdatedAt {
            return updatedAt > minimumUpdatedAt
        }
        return updatedAt > outage.since
    }

    private mutating func recordProgress(_ status: ComponentStatus, at now: Date) {
        lastProgressPublisherInstanceID = status.publisherEvidence?.publisherInstanceID
        lastProgressSequence = status.observationSequence
        lastProgressUpdatedAt = status.updatedAt
        lastProgressObservedAt = now
    }

    private func failureStatus(for outage: Outage, at now: Date) -> ComponentStatus {
        ComponentStatus(
            id: "managed-status-channel",
            schemaVersion: nil,
            health: .recovering,
            summary: L10n.text(.managedStatusChannelSummary),
            detail: outage.detail,
            updatedAt: outage.since,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: L10n.text(.managedStatusChannelReason),
            impact: L10n.text(.managedStatusChannelImpact),
            recommendations: [
                L10n.text(.managedStatusChannelWaitRecommendation),
                L10n.text(.openDashboardRecommendation),
            ],
            elapsedMilliseconds: Int64(max(0, now.timeIntervalSince(outage.since)) * 1_000),
            managedSessions: nil,
            observationSequence: outage.minimumSequence,
            publisherInstanceID: outage.minimumPublisherInstanceID,
            backendID: nil,
            mountPoint: nil,
            resourcePath: nil,
            recoveryEpochID: nil,
            recoveryEpochEstablishedAt: nil,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }

    private func recoveryStatus(for outage: Outage, status: ComponentStatus) -> ComponentStatus {
        ComponentStatus(
            id: "managed-status-channel",
            schemaVersion: nil,
            health: .healthy,
            summary: L10n.text(.managedStatusChannelRecoveredSummary),
            detail: "",
            updatedAt: status.updatedAt,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: nil,
            impact: nil,
            recommendations: [],
            elapsedMilliseconds: nil,
            managedSessions: status.managedSessions,
            observationSequence: status.observationSequence,
            publisherInstanceID: status.publisherInstanceID,
            backendID: status.backendID,
            mountPoint: status.mountPoint,
            resourcePath: status.resourcePath,
            recoveryEpochID: status.recoveryEpochID,
            recoveryEpochEstablishedAt: status.recoveryEpochEstablishedAt,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }
}

private struct SupervisorStatusChannelMonitor {
    private struct Outage {
        let id: String
        let since: Date
        var minimumPublisherInstanceID: String?
        var minimumSequence: UInt64?
        var minimumUpdatedAt: Date?
        var backendIdentity: BackendIdentity?
        var detail: String
        var reason: String
        var impact: String
        var recommendations: [String]
        var severe: Bool
    }

    private let freshness: TimeInterval
    private let futureTolerance: TimeInterval
    private var outage: Outage?
    private var lastProgressPublisherInstanceID: String?
    private var lastProgressSequence: UInt64?
    private var lastProgressUpdatedAt: Date?
    private var lastProgressObservedAt: Date?
    private var lastBackendIdentity: BackendIdentity?

    init(freshness: TimeInterval = 10, futureTolerance: TimeInterval = 2) {
        self.freshness = max(1, freshness)
        self.futureTolerance = max(0, futureTolerance)
    }

    mutating func evaluate(
        _ result: StatusChannelReadResult,
        continuity: StatusContinuityReadResult,
        at now: Date
    ) -> ComponentStatus? {
        guard case .valid(let status) = result, isStructurallyTrustworthy(status) else {
            let failureKind: StatusChannelReadFailureKind
            if case .unavailable(let failure) = result {
                failureKind = failure.kind
            } else {
                failureKind = .invalid
            }
            let authority = continuity.authority
            let baseDetail = failureKind == .missing
                ? L10n.text(.supervisorStatusChannelMissingDetail)
                : L10n.text(.supervisorStatusChannelInvalidDetail)
            let current = ensureOutage(
                id: authority?.occurrenceID(source: "supervisor-status-channel")
                    ?? statusChannelOccurrenceID(
                        source: "supervisor-status-channel",
                        status: nil,
                        continuity: continuity
                    ),
                since: authority.map { min($0.recoveryEpochEstablishedAt, now) } ?? now,
                evidence: nil,
                minimumUpdatedAt: nil,
                backendIdentity: nil,
                detail: authority == nil
                    ? "\(baseDetail)\n\(L10n.text(.supervisorStatusChannelContinuityInvalidDetail))"
                    : baseDetail,
                reason: L10n.text(.supervisorStatusChannelReason),
                impact: L10n.text(.supervisorStatusChannelImpact),
                recommendations: [
                    L10n.text(.supervisorStatusChannelWaitRecommendation),
                    L10n.text(.openDashboardRecommendation),
                ],
                severe: false
            )
            return failureStatus(for: current, at: now)
        }

        guard isFresh(status, at: now) else {
            let current = ensureOutage(
                id: statusChannelOccurrenceID(
                    source: "supervisor-status-channel",
                    status: status,
                    continuity: continuity
                ),
                since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
                evidence: status.publisherEvidence,
                minimumUpdatedAt: status.updatedAt,
                backendIdentity: status.backendIdentity,
                detail: L10n.text(.supervisorStatusChannelStaleDetail),
                reason: L10n.text(.supervisorStatusChannelReason),
                impact: L10n.text(.supervisorStatusChannelImpact),
                recommendations: [
                    L10n.text(.supervisorStatusChannelWaitRecommendation),
                    L10n.text(.openDashboardRecommendation),
                ],
                severe: false
            )
            return failureStatus(for: current, at: now)
        }
        let updatedAt = status.updatedAt ?? now

        if var current = outage {
            guard advances(status, beyond: current) else {
                return failureStatus(for: current, at: now)
            }
            if status.health == .failed {
                current.minimumPublisherInstanceID = status.publisherEvidence?.publisherInstanceID
                current.minimumSequence = status.publisherEvidence?.sequence
                current.minimumUpdatedAt = updatedAt
                current.backendIdentity = status.backendIdentity
                current.detail = status.detail.nonEmpty ?? status.summary
                current.reason = L10n.text(.supervisorRecoveryReason)
                current.impact = L10n.text(.supervisorFailureImpact)
                current.recommendations = [L10n.text(.retryRecommendation), L10n.text(.openDashboardRecommendation)]
                current.severe = true
                outage = current
                return failureStatus(for: current, at: now)
            }
            if status.health == .healthy,
               !statusChannelHasTrustedHealthyPublication(status, continuity: continuity) {
                current.detail = L10n.text(.supervisorStatusChannelContinuityInvalidDetail)
                outage = current
                return failureStatus(for: current, at: now)
            }
            outage = nil
            recordProgress(status, at: now)
            return recoveryStatus(for: current, status: status)
        }

        if status.health != .failed {
            if status.health == .healthy,
               !statusChannelHasTrustedHealthyPublication(status, continuity: continuity) {
                let current = ensureOutage(
                    id: statusChannelOccurrenceID(
                        source: "supervisor-status-channel",
                        status: status,
                        continuity: continuity
                    ),
                    since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
                    evidence: status.publisherEvidence,
                    minimumUpdatedAt: status.updatedAt,
                    backendIdentity: status.backendIdentity,
                    detail: L10n.text(.supervisorStatusChannelContinuityInvalidDetail),
                    reason: L10n.text(.supervisorStatusChannelReason),
                    impact: L10n.text(.supervisorStatusChannelImpact),
                    recommendations: [
                        L10n.text(.supervisorStatusChannelWaitRecommendation),
                        L10n.text(.openDashboardRecommendation),
                    ],
                    severe: false
                )
                return failureStatus(for: current, at: now)
            }
            if lastProgressObservedAt == nil || hasProgressed(status) {
                recordProgress(status, at: now)
                return nil
            }
            guard now.timeIntervalSince(lastProgressObservedAt ?? now) >= freshness else {
                return nil
            }
            let current = ensureOutage(
                id: statusChannelOccurrenceID(
                    source: "supervisor-status-channel",
                    status: status,
                    continuity: continuity
                ),
                since: lastProgressObservedAt ?? now,
                evidence: lastProgressEvidence,
                minimumUpdatedAt: lastProgressUpdatedAt,
                backendIdentity: lastBackendIdentity,
                detail: L10n.text(.supervisorStatusChannelStaleDetail),
                reason: L10n.text(.supervisorStatusChannelReason),
                impact: L10n.text(.supervisorStatusChannelImpact),
                recommendations: [
                    L10n.text(.supervisorStatusChannelWaitRecommendation),
                    L10n.text(.openDashboardRecommendation),
                ],
                severe: false
            )
            return failureStatus(for: current, at: now)
        }

        let current = ensureOutage(
            id: statusChannelOccurrenceID(
                source: "supervisor-status-channel",
                status: status,
                continuity: continuity
            ),
            since: statusChannelOccurrenceSince(status: status, continuity: continuity, at: now),
            evidence: status.publisherEvidence,
            minimumUpdatedAt: status.updatedAt,
            backendIdentity: status.backendIdentity,
            detail: status.detail.nonEmpty ?? status.summary,
            reason: L10n.text(.supervisorRecoveryReason),
            impact: L10n.text(.supervisorFailureImpact),
            recommendations: [L10n.text(.retryRecommendation), L10n.text(.openDashboardRecommendation)],
            severe: true
        )
        return failureStatus(for: current, at: now)
    }

    private mutating func ensureOutage(
        id: String,
        since: Date,
        evidence: PublisherEvidence?,
        minimumUpdatedAt: Date?,
        backendIdentity: BackendIdentity?,
        detail: String,
        reason: String,
        impact: String,
        recommendations: [String],
        severe: Bool
    ) -> Outage {
        if var current = outage {
            current.detail = detail
            current.reason = reason
            current.impact = impact
            current.recommendations = recommendations
            current.severe = current.severe || severe
            outage = current
            return current
        }
        let current = Outage(
            id: id,
            since: since,
            minimumPublisherInstanceID: evidence?.publisherInstanceID ?? lastProgressPublisherInstanceID,
            minimumSequence: evidence?.sequence ?? lastProgressSequence,
            minimumUpdatedAt: minimumUpdatedAt ?? lastProgressUpdatedAt,
            backendIdentity: backendIdentity ?? lastBackendIdentity,
            detail: detail,
            reason: reason,
            impact: impact,
            recommendations: recommendations,
            severe: severe
        )
        outage = current
        return current
    }

    private func advances(_ status: ComponentStatus, beyond outage: Outage) -> Bool {
        guard let candidate = status.publisherEvidence,
              let candidateBackend = status.backendIdentity else {
            return false
        }
        if let expectedBackend = outage.backendIdentity,
           !expectedBackend.matches(candidateBackend) {
            return false
        }
        if let minimumPublisherInstanceID = outage.minimumPublisherInstanceID {
            if candidate.publisherInstanceID == minimumPublisherInstanceID {
                guard let minimumSequence = outage.minimumSequence else { return false }
                return candidate.sequence > minimumSequence
            }
            return true
        }
        guard let updatedAt = status.updatedAt else { return false }
        return updatedAt > (outage.minimumUpdatedAt ?? outage.since)
    }

    private func isStructurallyTrustworthy(_ status: ComponentStatus) -> Bool {
        status.schemaVersion == 2
            && status.health != .unknown
            && status.publisherEvidence != nil
            && status.backendIdentity != nil
    }

    private func isFresh(_ status: ComponentStatus, at now: Date) -> Bool {
        guard let updatedAt = status.updatedAt,
              updatedAt <= now.addingTimeInterval(futureTolerance) else {
            return false
        }
        return now.timeIntervalSince(updatedAt) <= freshness
    }

    private func hasProgressed(_ status: ComponentStatus) -> Bool {
        guard let candidate = status.publisherEvidence,
              let candidateBackend = status.backendIdentity else {
            return false
        }
        if let lastBackendIdentity,
           !lastBackendIdentity.matches(candidateBackend) {
            return true
        }
        guard let lastProgressPublisherInstanceID else { return true }
        if candidate.publisherInstanceID != lastProgressPublisherInstanceID {
            return true
        }
        guard let lastProgressSequence else { return true }
        return candidate.sequence > lastProgressSequence
    }

    private mutating func recordProgress(_ status: ComponentStatus, at now: Date) {
        lastProgressPublisherInstanceID = status.publisherEvidence?.publisherInstanceID
        lastProgressSequence = status.publisherEvidence?.sequence
        lastProgressUpdatedAt = status.updatedAt
        lastProgressObservedAt = now
        lastBackendIdentity = status.backendIdentity
    }

    private var lastProgressEvidence: PublisherEvidence? {
        guard let lastProgressPublisherInstanceID,
              let lastProgressSequence else {
            return nil
        }
        return PublisherEvidence(
            publisherInstanceID: lastProgressPublisherInstanceID,
            sequence: lastProgressSequence
        )
    }

    private func failureStatus(for outage: Outage, at now: Date) -> ComponentStatus {
        let elapsedMilliseconds = Int64(max(0, now.timeIntervalSince(outage.since)) * 1_000)
        return ComponentStatus(
            id: "supervisor-status-channel",
            schemaVersion: nil,
            health: outage.severe && elapsedMilliseconds >= Int64(freshness * 1_000)
                ? .failed
                : .recovering,
            summary: L10n.text(.supervisorStatusChannelSummary),
            detail: outage.detail,
            updatedAt: outage.minimumUpdatedAt ?? outage.since,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: outage.reason,
            impact: outage.impact,
            recommendations: outage.recommendations,
            elapsedMilliseconds: elapsedMilliseconds,
            managedSessions: nil,
            observationSequence: outage.minimumSequence,
            publisherInstanceID: outage.minimumPublisherInstanceID,
            backendID: outage.backendIdentity?.backendID,
            mountPoint: outage.backendIdentity?.mountPoint,
            resourcePath: outage.backendIdentity?.resourcePath,
            recoveryEpochID: nil,
            recoveryEpochEstablishedAt: nil,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }

    private func recoveryStatus(for outage: Outage, status: ComponentStatus) -> ComponentStatus {
        ComponentStatus(
            id: "supervisor-status-channel",
            schemaVersion: nil,
            health: .healthy,
            summary: L10n.text(.supervisorStatusChannelRecoveredSummary),
            detail: "",
            updatedAt: status.updatedAt,
            incidentID: outage.id,
            incidentSince: outage.since,
            reason: nil,
            impact: nil,
            recommendations: [],
            elapsedMilliseconds: nil,
            managedSessions: nil,
            observationSequence: status.observationSequence,
            publisherInstanceID: status.publisherInstanceID,
            backendID: status.backendID,
            mountPoint: status.mountPoint,
            resourcePath: status.resourcePath,
            recoveryEpochID: status.recoveryEpochID,
            recoveryEpochEstablishedAt: status.recoveryEpochEstablishedAt,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }

}

private struct StatusLoadResult {
    let components: [ComponentStatus]
    let managedStatus: StatusChannelReadResult
    let daemonStatus: StatusChannelReadResult
    let supervisorStatus: StatusChannelReadResult
    let managedContinuity: StatusContinuityReadResult
    let daemonContinuity: StatusContinuityReadResult
    let supervisorContinuity: StatusContinuityReadResult
    let hasRuntimeEvidence: Bool
}

@MainActor
final class StatusStore: ObservableObject {
    @Published private(set) var components: [ComponentStatus] = []
    @Published private(set) var lastReadAt: Date?
    @Published private(set) var hostNotice: String?
    @Published private(set) var storageHistory: [StorageHistorySample]
    @Published private(set) var incidentHistory: [IncidentHistoryEntry]
    @Published private(set) var currentActivity: IOActivity?

    let localAuthorizationCapability: LocalAuthorizationCapability

    var onChange: (() -> Void)?
    var onIncident: ((IncidentUpdate) -> Void)?

    private let appGroupURL: URL?
    private let now: () -> Date
    private let monitorDaemonStatus: Bool
    private let alertsBeforeRuntimeObserved: Bool
    private let recordsHistory: Bool
    private var timer: Timer?
    private var historyArchive: StatusHistoryArchive
    private var previousActivityObservation: IOActivityObservation?
    private var incidentTracker: IncidentTracker
    private var daemonStatusChannel: DaemonStatusChannelMonitor
    private var managedStatusChannel: ManagedStatusChannelMonitor
    private var supervisorStatusChannel: SupervisorStatusChannelMonitor
    private var hasObservedRuntime = false

    init(
        appGroupURL: URL?,
        incidentTimeout: TimeInterval = 10,
        managedStatusFreshness: TimeInterval = 10,
        monitorDaemonStatus: Bool = false,
        alertsBeforeRuntimeObserved: Bool = true,
        recordsHistory: Bool = true,
        now: @escaping () -> Date = Date.init
    ) {
        self.appGroupURL = appGroupURL
        self.now = now
        self.monitorDaemonStatus = monitorDaemonStatus
        self.alertsBeforeRuntimeObserved = alertsBeforeRuntimeObserved
        self.recordsHistory = recordsHistory
        let historyArchive = StatusHistoryPersistence.load(appGroupURL: appGroupURL)
        self.historyArchive = historyArchive
        storageHistory = historyArchive.storageSamples
        incidentHistory = historyArchive.incidents
        currentActivity = nil
        localAuthorizationCapability = .current()
        incidentTracker = IncidentTracker(timeout: incidentTimeout)
        daemonStatusChannel = DaemonStatusChannelMonitor(freshness: managedStatusFreshness)
        managedStatusChannel = ManagedStatusChannelMonitor(freshness: managedStatusFreshness)
        supervisorStatusChannel = SupervisorStatusChannelMonitor(freshness: managedStatusFreshness)
    }

    var overallHealth: ComponentHealth {
        if incidentTracker.currentIncident != nil {
            return .failed
        }
        let critical = components.filter { component in
            let id = component.id.lowercased()
            return [
                "frontend", "native-fskit", "fskit-frontend", "managed",
                "supervisor", "daemon", "service", "backend",
            ].contains(id)
        }
        return critical.map(\.health).max() ?? .unknown
    }

    var currentIncident: FrontendIncident? {
        incidentTracker.currentIncident
    }

    var isRuntimeUnconnected: Bool {
        guard storageMetrics == nil else { return false }
        let runtime = components.filter { component in
            let id = component.id.lowercased()
            return [
                "frontend", "native-fskit", "fskit-frontend", "managed",
                "supervisor", "daemon", "service", "backend",
            ].contains(id)
        }
        return !runtime.isEmpty
            && runtime.allSatisfy { $0.health == .unknown && $0.updatedAt == nil }
    }

    var storageMetrics: StorageMetrics? {
        StorageMetrics.current(from: components)
    }

    var menuBarTitle: String {
        guard let storageMetrics else { return "" }
        return "↓\(Int((storageMetrics.savingsFraction * 100).rounded()))%"
    }

    var summary: String {
        switch overallHealth {
        case .healthy: return L10n.text(.statusSummaryHealthy)
        case .recovering: return L10n.text(.statusSummaryRecovering)
        case .failed: return L10n.text(.statusSummaryAttention)
        case .unknown: return L10n.text(.statusSummaryUnavailable)
        }
    }

    var statusDirectoryURL: URL? {
        guard let appGroupURL else { return nil }
        let statusDirectory = appGroupURL.appendingPathComponent("status", isDirectory: true)
        var isDirectory: ObjCBool = false
        if FileManager.default.fileExists(atPath: statusDirectory.path, isDirectory: &isDirectory), isDirectory.boolValue {
            return statusDirectory
        }
        return appGroupURL
    }

    func start() {
        refresh()
        let timer = Timer(timeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        persistHistory()
    }

    func refresh() {
        let observedAt = now()
        let loaded = loadStatuses(at: observedAt)
        hasObservedRuntime = hasObservedRuntime || loaded.hasRuntimeEvidence
        let monitorsRuntime = alertsBeforeRuntimeObserved || hasObservedRuntime
        components = loaded.components
        lastReadAt = observedAt
        currentActivity = updateIOActivity()
        let frontend = componentStatus(aliases: ["frontend", "native-fskit", "fskit-frontend"])
        let daemonChannel = monitorDaemonStatus && monitorsRuntime
            ? daemonStatusChannel.evaluate(
                loaded.daemonStatus,
                continuity: loaded.daemonContinuity,
                at: observedAt
            )
            : nil
        let managed = loaded.managedStatus.status
        let managedChannel = monitorsRuntime
            ? managedStatusChannel.evaluate(
                loaded.managedStatus,
                continuity: loaded.managedContinuity,
                at: observedAt
            )
            : nil
        let supervisor = loaded.supervisorStatus.status
        let supervisorChannel = monitorsRuntime
            ? supervisorStatusChannel.evaluate(
                loaded.supervisorStatus,
                continuity: loaded.supervisorContinuity,
                at: observedAt
            )
            : nil
        let incidentUpdate = incidentTracker.evaluate(
            frontend: frontend,
            managed: managed,
            daemonStatusChannel: daemonChannel,
            managedStatusChannel: managedChannel,
            supervisorStatusChannel: supervisorChannel,
            supervisor: supervisor,
            at: observedAt
        )
        if recordsHistory {
            let insertedMinute = historyArchive.recordStorage(
                storageMetrics,
                activity: currentActivity,
                health: overallHealth,
                at: observedAt
            )
            var shouldPersist = insertedMinute
            if let incidentUpdate {
                shouldPersist = historyArchive.recordIncident(incidentUpdate, at: observedAt) || shouldPersist
            }
            storageHistory = historyArchive.storageSamples
            incidentHistory = historyArchive.incidents
            if shouldPersist { persistHistory() }
        }
        if let incidentUpdate {
            onIncident?(incidentUpdate)
        }
        onChange?()
    }

    func reportHostNotice(_ message: String?) {
        hostNotice = message
        onChange?()
    }

    private func persistHistory() {
        guard recordsHistory else { return }
        try? StatusHistoryPersistence.save(historyArchive, appGroupURL: appGroupURL)
    }

    func diagnosticPayload(incident: FrontendIncident? = nil) -> Data? {
        let formatter = ISO8601DateFormatter()
        var payload: [String: Any] = [
            "schema_version": 2,
            "generated_at": formatter.string(from: Date()),
            "application": "CodexFoldFSKit",
            "redaction": "conservative-structured",
            "local_authorization": [
                "administrator_group_membership": localAuthorizationCapability.administratorMembership.rawValue,
                "sudo_cache": localAuthorizationCapability.sudoCacheState,
                "automatic_elevation": localAuthorizationCapability.automaticElevationAllowed,
                "explicit_user_action_required": localAuthorizationCapability.explicitUserActionRequired,
            ],
            "components": components.map { component in
                var value: [String: Any] = [
                    "component": redactDiagnosticText(component.id),
                    "state": diagnosticHealth(component.health),
                ]
                if let updatedAt = component.updatedAt {
                    value["updated_at"] = formatter.string(from: updatedAt)
                }
                if let elapsed = component.elapsedMilliseconds {
                    value["elapsed_milliseconds"] = elapsed
                }
                if let sessions = component.managedSessions {
                    value["managed_sessions"] = sessions
                }
                if let sequence = component.observationSequence {
                    value["observation_sequence"] = NSNumber(value: sequence)
                }
                if let logicalBytes = component.logicalBytes {
                    value["logical_bytes"] = logicalBytes
                }
                if let physicalBytes = component.physicalBytes {
                    value["physical_bytes"] = physicalBytes
                }
                if let readBytesTotal = component.readBytesTotal {
                    value["read_bytes_total"] = NSNumber(value: readBytesTotal)
                }
                if let writtenBytesTotal = component.writtenBytesTotal {
                    value["written_bytes_total"] = NSNumber(value: writtenBytesTotal)
                }
                return value
            },
        ]
        if let incident {
            var incidentPayload: [String: Any] = [
                "id": redactDiagnosticText(incident.id),
                "since": formatter.string(from: incident.since),
                "reason": redactDiagnosticText(incident.reason),
                "impact": redactDiagnosticText(incident.impact),
                "recommendations": incident.recommendations.map(redactDiagnosticText),
                "occurrence_continuity": incident.id.contains("-unverified-")
                    ? "unverified"
                    : "verified_or_publisher",
            ]
            if let recoveredAt = incident.recoveredAt {
                incidentPayload["recovered_at"] = formatter.string(from: recoveredAt)
            }
            payload["incident"] = incidentPayload
        }
        return try? JSONSerialization.data(withJSONObject: payload, options: [.prettyPrinted, .sortedKeys])
    }

    private func componentStatus(aliases: Set<String>) -> ComponentStatus? {
        components.first { aliases.contains($0.id.lowercased()) }
    }

    private func updateIOActivity() -> IOActivity? {
        guard let component = components.first(where: {
            $0.readBytesTotal != nil && $0.writtenBytesTotal != nil
        }),
              let capturedAt = component.updatedAt,
              let publisherInstanceID = component.publisherInstanceID,
              let readBytesTotal = component.readBytesTotal,
              let writtenBytesTotal = component.writtenBytesTotal else {
            return nil
        }
        let observation = IOActivityObservation(
            capturedAt: capturedAt,
            publisherInstanceID: publisherInstanceID,
            readBytesTotal: readBytesTotal,
            writtenBytesTotal: writtenBytesTotal
        )
        guard observation != previousActivityObservation else { return currentActivity }
        defer { previousActivityObservation = observation }
        guard let previousActivityObservation,
              previousActivityObservation.publisherInstanceID == observation.publisherInstanceID,
              observation.capturedAt > previousActivityObservation.capturedAt,
              observation.readBytesTotal >= previousActivityObservation.readBytesTotal,
              observation.writtenBytesTotal >= previousActivityObservation.writtenBytesTotal else {
            return nil
        }
        let interval = observation.capturedAt.timeIntervalSince(previousActivityObservation.capturedAt)
        guard interval > 0 else { return nil }
        return IOActivity(
            capturedAt: observation.capturedAt,
            readBytesPerSecond: Double(observation.readBytesTotal - previousActivityObservation.readBytesTotal) / interval,
            writtenBytesPerSecond: Double(observation.writtenBytesTotal - previousActivityObservation.writtenBytesTotal) / interval
        )
    }

    private func loadStatuses(at now: Date) -> StatusLoadResult {
        guard let appGroupURL else {
            let missing = StatusChannelReadResult.unavailable(
                StatusChannelReadFailure(kind: .missing)
            )
            return StatusLoadResult(
                components: placeholderStatuses(),
                managedStatus: missing,
                daemonStatus: missing,
                supervisorStatus: missing,
                managedContinuity: .unavailable,
                daemonContinuity: .unavailable,
                supervisorContinuity: .unavailable,
                hasRuntimeEvidence: false
            )
        }
        let statusDirectory = appGroupURL.appendingPathComponent("status", isDirectory: true)
        let files = (try? FileManager.default.contentsOfDirectory(
                at: statusDirectory,
                includingPropertiesForKeys: nil,
                options: [.skipsHiddenFiles]
            )) ?? []

        let managedURL = statusDirectory.appendingPathComponent("managed.json", isDirectory: false)
        let daemonURL = statusDirectory.appendingPathComponent("daemon.json", isDirectory: false)
        let supervisorURL = statusDirectory.appendingPathComponent("supervisor.json", isDirectory: false)
        let managedStatus = loadManagedStatus(at: managedURL, now: now)
        let daemonStatus = loadStatusChannel(at: daemonURL, component: "daemon", now: now)
        let supervisorStatus = loadStatusChannel(at: supervisorURL, component: "supervisor", now: now)
        let excludedStatusFiles = Set([
            managedURL.lastPathComponent,
            daemonURL.lastPathComponent,
            supervisorURL.lastPathComponent,
        ])
        var statuses = files
            .filter {
                $0.pathExtension.lowercased() == "json"
                    && !excludedStatusFiles.contains($0.lastPathComponent)
            }
            .compactMap { parseStatus($0, now: now) }
            .reduce(into: [String: ComponentStatus]()) { result, status in
                if let existing = result[status.id],
                   (existing.updatedAt ?? .distantPast) >= (status.updatedAt ?? .distantPast) {
                    return
                }
                result[status.id] = status
            }
        statuses["managed"] = managedStatus.status ?? placeholderStatus("managed")
        statuses["daemon"] = daemonStatus.status ?? placeholderStatus("daemon")
        statuses["supervisor"] = supervisorStatus.status ?? placeholderStatus("supervisor")
        for component in ["frontend", "managed", "supervisor", "daemon"] where statuses[component] == nil {
            statuses[component] = placeholderStatus(component)
        }
        let managedContinuity = loadStatusContinuity(component: "managed", appGroupURL: appGroupURL)
        let daemonContinuity = loadStatusContinuity(component: "daemon", appGroupURL: appGroupURL)
        let supervisorContinuity = loadStatusContinuity(component: "supervisor", appGroupURL: appGroupURL)
        return StatusLoadResult(
            components: statuses.values.sorted { lhs, rhs in
                if lhs.health != rhs.health { return lhs.health > rhs.health }
                return lhs.id.localizedStandardCompare(rhs.id) == .orderedAscending
            },
            managedStatus: managedStatus,
            daemonStatus: daemonStatus,
            supervisorStatus: supervisorStatus,
            managedContinuity: managedContinuity,
            daemonContinuity: daemonContinuity,
            supervisorContinuity: supervisorContinuity,
            hasRuntimeEvidence: !files.isEmpty
                || managedContinuity.authority != nil
                || daemonContinuity.authority != nil
                || supervisorContinuity.authority != nil
        )
    }

    private func loadManagedStatus(at url: URL, now: Date) -> StatusChannelReadResult {
        do {
            let data = try readBoundedRegularFile(at: url)
            guard let root = decodeStatusRoot(data),
                  let schemaVersion = integer(root, keys: ["schemaVersion", "schema_version"]),
                  schemaVersion == 1 || schemaVersion == 2,
                  string(root, keys: ["component"])?.lowercased() == "managed",
                  let rawState = string(root, keys: ["state"]),
                  parseHealth(rawState, healthy: nil) != .unknown,
                  let status = parseStatus(root, fallbackComponent: "managed", now: now),
                  status.updatedAt != nil else {
                return .unavailable(StatusChannelReadFailure(kind: .invalid))
            }
            if schemaVersion == 2 {
                guard status.publisherEvidence != nil,
                      status.backendID?.nonEmpty != nil,
                      let mountPoint = status.mountPoint,
                      let resourcePath = status.resourcePath,
                      (mountPoint as NSString).isAbsolutePath,
                      (resourcePath as NSString).isAbsolutePath else {
                    return .unavailable(StatusChannelReadFailure(kind: .invalid))
                }
            }
            let hasIncidentID = status.incidentID != nil
            let hasIncidentSince = status.incidentSince != nil
            if hasIncidentID != hasIncidentSince {
                return .unavailable(StatusChannelReadFailure(kind: .invalid))
            }
            if status.health == .recovering || status.health == .failed {
                guard hasIncidentID, hasIncidentSince else {
                    return .unavailable(StatusChannelReadFailure(kind: .invalid))
                }
            }
            return .valid(status)
        } catch let error as POSIXError where error.code == .ENOENT {
            return .unavailable(StatusChannelReadFailure(kind: .missing))
        } catch {
            return .unavailable(StatusChannelReadFailure(kind: .invalid))
        }
    }

    private func loadStatusChannel(
        at url: URL,
        component: String,
        now: Date
    ) -> StatusChannelReadResult {
        do {
            let data = try readBoundedRegularFile(at: url)
            guard let root = decodeStatusRoot(data),
                  let status = parseStatus(root, fallbackComponent: component, now: now),
                  canonicalComponentIdentifier(status.id) == component else {
                return .unavailable(StatusChannelReadFailure(kind: .invalid))
            }
            return .valid(status)
        } catch let error as POSIXError where error.code == .ENOENT {
            return .unavailable(StatusChannelReadFailure(kind: .missing))
        } catch {
            return .unavailable(StatusChannelReadFailure(kind: .invalid))
        }
    }

    private func loadStatusContinuity(
        component: String,
        appGroupURL: URL
    ) -> StatusContinuityReadResult {
        let url = appGroupURL
            .appendingPathComponent("status-continuity", isDirectory: true)
            .appendingPathComponent("\(component).json", isDirectory: false)
        guard let data = try? readBoundedRegularFile(at: url),
              let root = decodeStatusRoot(data),
              integer(root, keys: ["schemaVersion", "schema_version"]) == 2,
              string(root, keys: ["component"])?.lowercased() == component,
              ["bootstrap", "healthy"].contains(string(root, keys: ["state"])?.lowercased() ?? ""),
              let updatedAt = date(root, keys: ["updatedAt", "updated_at"]),
              let recoveryEpochID = string(root, keys: ["recoveryEpochID", "recovery_epoch_id"]),
              let recoveryEpochEstablishedAt = date(
                  root,
                  keys: ["recoveryEpochEstablishedAt", "recovery_epoch_established_at"]
              ),
              let continuitySequence = uint64(root, keys: ["generation"]),
              continuitySequence > 0,
              let publisherInstanceID = string(root, keys: ["publisherInstanceID", "publisher_instance_id"]),
              let observationSequence = uint64(root, keys: ["observationSequence", "observation_sequence"]),
              observationSequence > 0,
              let backendID = string(root, keys: ["backendID", "backend_id"]),
              let mountPoint = string(root, keys: ["mountPoint", "mount_point"]),
              let resourcePath = string(root, keys: ["resourcePath", "resource_path"]),
              (mountPoint as NSString).isAbsolutePath,
              (resourcePath as NSString).isAbsolutePath,
              updatedAt == recoveryEpochEstablishedAt else {
            return .unavailable
        }
        return .verified(
            StatusContinuityAuthority(
                component: component,
                recoveryEpochID: recoveryEpochID,
                recoveryEpochEstablishedAt: recoveryEpochEstablishedAt,
                continuitySequence: continuitySequence,
                publisherEvidence: PublisherEvidence(
                    publisherInstanceID: publisherInstanceID,
                    sequence: observationSequence
                ),
                backendIdentity: BackendIdentity(
                    backendID: backendID,
                    mountPoint: mountPoint,
                    resourcePath: resourcePath
                )
            )
        )
    }

    private func parseStatus(_ url: URL, now: Date) -> ComponentStatus? {
        guard let data = try? readBoundedRegularFile(at: url),
              let root = decodeStatusRoot(data) else {
            return nil
        }
        let fileComponent = url.deletingPathExtension().lastPathComponent
        guard let status = parseStatus(root, fallbackComponent: fileComponent, now: now),
              canonicalComponentIdentifier(status.id) == canonicalComponentIdentifier(fileComponent) else {
            return nil
        }
        return status
    }

    private func decodeStatusRoot(_ data: Data) -> [String: Any]? {
        guard data.count <= 1_048_576,
              let object = try? JSONSerialization.jsonObject(with: data),
              let root = object as? [String: Any] else {
            return nil
        }
        return root
    }

    private func parseStatus(
        _ root: [String: Any],
        fallbackComponent: String,
        now: Date
    ) -> ComponentStatus? {
        let incident = root["incident"] as? [String: Any]
        let component = string(root, keys: ["component", "name", "subsystem"])
            ?? fallbackComponent
        let rawState = string(root, keys: ["state", "status", "health"])
        let healthy = root["healthy"] as? Bool
        let health = parseHealth(rawState, healthy: healthy)
        let summary = string(root, keys: ["summary", "message"])
            ?? localizedComponentName(component)
        let detail = string(root, keys: ["detail", "details", "lastTransportError", "error"])
            ?? string(incident, keys: ["technical_details", "detail", "error"])
            ?? ""
        let updatedAt = date(root, keys: ["updatedAt", "updated_at", "observedAt", "observed_at", "timestamp"])
        let reason = string(incident, keys: ["reason", "summary"])
            ?? string(root, keys: ["reason", "lastTransportError"])
        let impact = string(incident, keys: ["impact"])
            ?? string(root, keys: ["impact"])
        let recommendations = stringArray(incident, keys: ["recommendations", "recommended_actions"])
            ?? stringArray(root, keys: ["recommendations", "recommended_actions"])
            ?? []
        let elapsedMilliseconds = int64(root, keys: ["elapsedMilliseconds", "elapsed_milliseconds"])
        let recoveryStartedAt = date(root, keys: ["recoveryStartedAt", "recovery_started_at", "unhealthySince", "unhealthy_since", "failedSince", "failed_since"])
            ?? elapsedMilliseconds.map { now.addingTimeInterval(-Double($0) / 1_000) }
        return ComponentStatus(
            id: component,
            schemaVersion: integer(root, keys: ["schemaVersion", "schema_version"]),
            health: health,
            summary: summary,
            detail: detail,
            updatedAt: updatedAt,
            incidentID: string(incident, keys: ["id", "incidentID", "incident_id"])
                ?? string(root, keys: ["incidentID", "incident_id"]),
            incidentSince: date(incident, keys: ["since", "startedAt", "started_at"])
                ?? recoveryStartedAt,
            reason: reason,
            impact: impact,
            recommendations: recommendations,
            elapsedMilliseconds: elapsedMilliseconds,
            managedSessions: integer(root, keys: ["managedSessions", "managed_sessions", "session_count"]),
            observationSequence: uint64(root, keys: ["observationSequence", "observation_sequence"]),
            publisherInstanceID: string(root, keys: ["publisherInstanceID", "publisher_instance_id"]),
            backendID: string(root, keys: ["backendID", "backend_id"]),
            mountPoint: string(root, keys: ["mountPoint", "mount_point"]),
            resourcePath: string(root, keys: ["resourcePath", "resource_path"]),
            recoveryEpochID: string(root, keys: ["recoveryEpochID", "recovery_epoch_id"]),
            recoveryEpochEstablishedAt: date(
                root,
                keys: ["recoveryEpochEstablishedAt", "recovery_epoch_established_at"]
            ),
            logicalBytes: int64(root, keys: ["logical_bytes"]),
            physicalBytes: int64(root, keys: ["physical_bytes", "store_bytes"]),
            readBytesTotal: uint64(root, keys: ["read_bytes_total"]),
            writtenBytesTotal: uint64(root, keys: ["written_bytes_total"])
        )
    }

    private func parseHealth(_ value: String?, healthy: Bool?) -> ComponentHealth {
        if healthy == true { return .healthy }
        if healthy == false { return .failed }
        switch value?.lowercased() {
        case "healthy", "ok", "ready", "running", "available": return .healthy
        case "recovering", "degraded", "starting", "waiting", "retrying": return .recovering
        case "failed", "failure", "critical", "error", "unhealthy", "unavailable", "stopped": return .failed
        default: return .unknown
        }
    }

    private func diagnosticHealth(_ health: ComponentHealth) -> String {
        switch health {
        case .healthy: return "healthy"
        case .unknown: return "unknown"
        case .recovering: return "recovering"
        case .failed: return "failed"
        }
    }

    private func localizedComponentName(_ name: String) -> String {
        switch name.lowercased() {
        case "frontend", "native-fskit", "fskit-frontend": return L10n.text(.frontend)
        case "managed": return L10n.text(.managed)
        case "supervisor": return L10n.text(.supervisor)
        case "daemon", "service", "backend": return L10n.text(.daemon)
        case "storage", "store": return L10n.text(.storage)
        default: return name
        }
    }

    private func canonicalComponentIdentifier(_ name: String) -> String {
        switch name.lowercased() {
        case "frontend", "native-fskit", "fskit-frontend": return "frontend"
        case "daemon", "service", "backend": return "daemon"
        case "storage", "store": return "storage"
        default: return name.lowercased()
        }
    }

    private func placeholderStatuses() -> [ComponentStatus] {
        ["frontend", "managed", "supervisor", "daemon"].map(placeholderStatus)
    }

    private func placeholderStatus(_ component: String) -> ComponentStatus {
        ComponentStatus(
            id: component,
            schemaVersion: nil,
            health: .unknown,
            summary: localizedComponentName(component),
            detail: L10n.text(.neverUpdated),
            updatedAt: nil,
            incidentID: nil,
            incidentSince: nil,
            reason: nil,
            impact: nil,
            recommendations: [],
            elapsedMilliseconds: nil,
            managedSessions: nil,
            observationSequence: nil,
            publisherInstanceID: nil,
            backendID: nil,
            mountPoint: nil,
            resourcePath: nil,
            recoveryEpochID: nil,
            recoveryEpochEstablishedAt: nil,
            logicalBytes: nil,
            physicalBytes: nil,
            readBytesTotal: nil,
            writtenBytesTotal: nil
        )
    }

    private func string(_ object: [String: Any]?, keys: [String]) -> String? {
        guard let object else { return nil }
        for key in keys {
            if let value = object[key] as? String, !value.isEmpty { return value }
        }
        return nil
    }

    private func stringArray(_ object: [String: Any]?, keys: [String]) -> [String]? {
        guard let object else { return nil }
        for key in keys {
            if let value = object[key] as? [String] { return value }
        }
        return nil
    }

    private func date(_ object: [String: Any]?, keys: [String]) -> Date? {
        guard let raw = string(object, keys: keys) else { return nil }
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return fractional.date(from: raw) ?? ISO8601DateFormatter().date(from: raw)
    }

    private func integer(_ object: [String: Any]?, keys: [String]) -> Int? {
        guard let object else { return nil }
        for key in keys {
            if let value = object[key] as? NSNumber { return value.intValue }
        }
        return nil
    }

    private func int64(_ object: [String: Any]?, keys: [String]) -> Int64? {
        guard let object else { return nil }
        for key in keys {
            if let value = object[key] as? NSNumber { return value.int64Value }
        }
        return nil
    }

    private func uint64(_ object: [String: Any]?, keys: [String]) -> UInt64? {
        guard let object else { return nil }
        for key in keys {
            if let value = object[key] as? NSNumber {
                let signed = value.int64Value
                if signed >= 0 { return UInt64(signed) }
            }
        }
        return nil
    }

    private func readBoundedRegularFile(at url: URL) throws -> Data {
        let directoryURL = url.deletingLastPathComponent()
        let directoryDescriptor = directoryURL.path.withCString {
            Darwin.open($0, O_RDONLY | O_CLOEXEC | O_DIRECTORY | O_NOFOLLOW)
        }
        guard directoryDescriptor >= 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }
        defer { Darwin.close(directoryDescriptor) }

        let descriptor = url.lastPathComponent.withCString {
            Darwin.openat(directoryDescriptor, $0, O_RDONLY | O_CLOEXEC | O_NOFOLLOW | O_NONBLOCK)
        }
        guard descriptor >= 0 else {
            throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
        }

        var info = stat()
        guard Darwin.fstat(descriptor, &info) == 0 else {
            let code = errno
            Darwin.close(descriptor)
            throw POSIXError(POSIXErrorCode(rawValue: code) ?? .EIO)
        }
        guard (info.st_mode & S_IFMT) == S_IFREG,
              info.st_size >= 0,
              info.st_size <= 1_048_576 else {
            Darwin.close(descriptor)
            throw POSIXError(.EINVAL)
        }

        let handle = FileHandle(fileDescriptor: descriptor, closeOnDealloc: true)
        defer { try? handle.close() }
        let data = try handle.read(upToCount: 1_048_577) ?? Data()
        guard data.count <= 1_048_576 else {
            throw POSIXError(.EFBIG)
        }
        var after = stat()
        guard Darwin.fstat(descriptor, &after) == 0,
              (after.st_mode & S_IFMT) == S_IFREG,
              after.st_dev == info.st_dev,
              after.st_ino == info.st_ino,
              after.st_size == info.st_size,
              after.st_size == data.count,
              after.st_mtimespec.tv_sec == info.st_mtimespec.tv_sec,
              after.st_mtimespec.tv_nsec == info.st_mtimespec.tv_nsec,
              after.st_ctimespec.tv_sec == info.st_ctimespec.tv_sec,
              after.st_ctimespec.tv_nsec == info.st_ctimespec.tv_nsec else {
            throw POSIXError(.EIO)
        }
        return data
    }
}

func redactDiagnosticText(_ value: String) -> String {
    var result = value
    let replacements: [(pattern: String, template: String)] = [
        (#"(?i)\b[A-Za-z][A-Za-z0-9+.-]*://[^\s<>\"']+"#, "<redacted-url>"),
        (#"(?i)(bearer\s+)[A-Za-z0-9._~+/=-]+"#, "$1<redacted>"),
        (#"(?i)((?:token|secret|password|api[_-]?key)\s*[:=]\s*)[^\s,;]+"#, "$1<redacted>"),
        (#"(?<!\S)~/(?:[^\r\n,;]+)"#, "<redacted-path>"),
        (#"(?<![A-Za-z0-9._~-])/(?:[^\r\n,;]+)"#, "<redacted-path>"),
    ]
    for replacement in replacements {
        guard let regex = try? NSRegularExpression(pattern: replacement.pattern) else { continue }
        let range = NSRange(result.startIndex..., in: result)
        result = regex.stringByReplacingMatches(
            in: result,
            range: range,
            withTemplate: replacement.template
        )
    }
    return result
}

private extension String {
    var nonEmpty: String? { isEmpty ? nil : self }
}
