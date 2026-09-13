import Foundation

enum EnrollmentCheckInterval: String, CaseIterable, Identifiable {
    case thirtyMinutes = "30m"
    case oneHour = "1h"
    case twoHours = "2h"

    var id: String { rawValue }

    var duration: TimeInterval {
        switch self {
        case .thirtyMinutes: return 30 * 60
        case .oneHour: return 60 * 60
        case .twoHours: return 2 * 60 * 60
        }
    }

    var title: String {
        switch self {
        case .thirtyMinutes: return L10n.text(.durationThirtyMinutes)
        case .oneHour: return L10n.text(.rangeOneHour)
        case .twoHours: return L10n.text(.durationTwoHours)
        }
    }

    static func matching(_ raw: String) -> EnrollmentCheckInterval? {
        allCases.first { $0.rawValue == raw || $0.durationString == raw }
    }

    var durationString: String {
        switch self {
        case .thirtyMinutes: return "30m0s"
        case .oneHour: return "1h0m0s"
        case .twoHours: return "2h0m0s"
        }
    }
}

enum EnrollmentIdleDuration: String, CaseIterable, Identifiable {
    case oneHour = "1h"
    case sixHours = "6h"
    case oneDay = "24h"

    var id: String { rawValue }

    var title: String {
        switch self {
        case .oneHour: return L10n.text(.rangeOneHour)
        case .sixHours: return L10n.text(.durationSixHours)
        case .oneDay: return L10n.text(.durationOneDay)
        }
    }

    static func matching(_ raw: String) -> EnrollmentIdleDuration? {
        allCases.first { $0.rawValue == raw || $0.durationString == raw }
    }

    var durationString: String {
        switch self {
        case .oneHour: return "1h0m0s"
        case .sixHours: return "6h0m0s"
        case .oneDay: return "24h0m0s"
        }
    }
}

/// How many sessions one pass folds.
///
/// A pass rebuilds the whole pack whether it folds one session or two hundred,
/// so this is the difference between paying that rebuild once per session and
/// paying it once per two hundred. Automatic sizes it from what recent passes
/// measured; the fixed choices are for when you would rather decide.
enum EnrollmentBatchSize: Int, CaseIterable, Identifiable {
    case automatic = 0
    case ten = 10
    case twentyFive = 25
    case fifty = 50
    case oneHundred = 100
    case twoHundred = 200

    var id: Int { rawValue }

    var title: String {
        switch self {
        case .automatic: return L10n.text(.autoFoldBatchAutomatic)
        case .ten, .twentyFive, .fifty, .oneHundred, .twoHundred:
            return L10n.format(.autoFoldBatchCountFormat, rawValue)
        }
    }

    /// Any stored number that is not one of the offered choices still has to
    /// round-trip, so it snaps to the nearest one rather than being discarded.
    static func matching(_ stored: Int) -> EnrollmentBatchSize {
        if stored <= 0 { return .automatic }
        var best = EnrollmentBatchSize.ten
        for option in allCases where option != .automatic {
            if abs(option.rawValue - stored) < abs(best.rawValue - stored) {
                best = option
            }
        }
        return best
    }
}

struct EnrollmentSettings: Equatable {
    var enabled: Bool
    var interval: EnrollmentCheckInterval
    var idleFor: EnrollmentIdleDuration
    var archivedOnly: Bool
    var batchSize: EnrollmentBatchSize

    static let `default` = EnrollmentSettings(
        enabled: false,
        interval: .thirtyMinutes,
        idleFor: .oneHour,
        archivedOnly: true,
        batchSize: .automatic
    )
}

struct EnrollmentPolicyFile: Equatable, Codable {
    var version: Int
    var enabled: Bool
    var interval: String
    var stableFor: String
    var archivedOnly: Bool
    var batchSize: Int

    enum CodingKeys: String, CodingKey {
        case version
        case enabled
        case interval
        case stableFor = "stable_for"
        case archivedOnly = "archived_only"
        case batchSize = "batch_size"
    }

    var settings: EnrollmentSettings {
        EnrollmentSettings(
            enabled: enabled,
            interval: EnrollmentCheckInterval.matching(interval) ?? .thirtyMinutes,
            idleFor: EnrollmentIdleDuration.matching(stableFor) ?? .oneHour,
            archivedOnly: archivedOnly,
            batchSize: EnrollmentBatchSize.matching(batchSize)
        )
    }

    /// Rebuilds the policy from what the user can actually see and change,
    /// carrying forward the fields this app does not model.
    ///
    /// How many sessions a cycle folds is one of those: there is no control for
    /// it here, and a cycle rebuilds the whole pack whether it folds one session
    /// or fifty. Writing a fixed 1 back meant that flipping any switch in this
    /// pane silently reset a tuned batch and made every later cycle pay a full
    /// pack rebuild per session.
    static func from(_ settings: EnrollmentSettings, preserving existing: EnrollmentPolicyFile?) -> EnrollmentPolicyFile {
        // The pane owns this field now, but it can only show the sizes it offers.
        // A stored number it rounded to the nearest choice must survive a change
        // to some *other* setting: writing the rounded value back would quietly
        // retune a batch the user never touched, which is exactly how a tuned
        // batch got reset to one every time a switch was flipped.
        var batchSize = settings.batchSize.rawValue
        if let stored = existing?.batchSize, EnrollmentBatchSize.matching(stored) == settings.batchSize {
            batchSize = stored
        }
        return EnrollmentPolicyFile(
            version: 1,
            enabled: settings.enabled,
            interval: settings.interval.rawValue,
            stableFor: settings.idleFor.rawValue,
            archivedOnly: settings.archivedOnly,
            batchSize: max(0, batchSize)
        )
    }

    static func load(from storeURL: URL) -> EnrollmentPolicyFile? {
        let url = storeURL.appendingPathComponent("enrollment/policy.json", isDirectory: false)
        guard let data = try? DurableAppGroupFile.read(url) else { return nil }
        return try? JSONDecoder().decode(EnrollmentPolicyFile.self, from: data)
    }

    func write(to storeURL: URL) throws {
        let directory = storeURL.appendingPathComponent("enrollment", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let url = directory.appendingPathComponent("policy.json", isDirectory: false)
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        var data = try encoder.encode(self)
        data.append(0x0A)
        try DurableAppGroupFile.write(data, to: url)
    }
}

struct EnrollmentProgress: Equatable {
    var storePath: String
    var enabled: Bool
    var interval: String
    var stableFor: String
    var archivedOnly: Bool
    var phase: String
    var managedCount: Int
    var waitingCount: Int
    var waitingKnown: Bool
    var cycleTotal: Int
    var cycleDone: Int
    var nextCheckAt: Date?
    var lastError: String
    var errorKind: String
    var updatedAt: Date?

    static let empty = EnrollmentProgress(
        storePath: "",
        enabled: false,
        interval: "",
        stableFor: "",
        archivedOnly: true,
        phase: "",
        managedCount: 0,
        waitingCount: 0,
        waitingKnown: false,
        cycleTotal: 0,
        cycleDone: 0,
        nextCheckAt: nil,
        lastError: "",
        errorKind: "",
        updatedAt: nil
    )

    var isWorking: Bool {
        ["checking", "folding", "packing", "migrating", "reclaiming"].contains(phase)
    }

    var fraction: Double? {
        if phase == "waiting-reclaim" {
            return cycleTotal > 0 ? Double(cycleDone) / Double(cycleTotal) : 0
        }
        if isWorking {
            guard cycleTotal > 0 else { return nil }
            return Double(cycleDone) / Double(cycleTotal)
        }
        if waitingKnown {
            let total = managedCount + waitingCount
            if total == 0 { return enabled ? 1 : 0 }
            return Double(managedCount) / Double(total)
        }
        // Waiting for the first scan is idle, not an indeterminate operation.
        return 0
    }

    func caption(now: Date) -> String {
        if errorKind == "configuration" && !isWorking {
            return L10n.text(.autoFoldConfigurationError)
        }
        if !enabled || phase == "disabled" || phase.isEmpty && !enabled {
            return L10n.text(.autoFoldOff)
        }
        var parts: [String] = []
        // A run in flight still knows how much is folded and how much is left,
        // and that is the part worth reading. Returning the bare phase word made
        // a pass working through thousands of sessions look like nothing was
        // moving, which is exactly when the counts matter most.
        switch phase {
        case "checking":
            parts.append(L10n.text(.autoFoldChecking))
        case "reclaiming":
            parts.append(L10n.text(.autoFoldReclaiming))
        case "waiting-reclaim":
            parts.append(L10n.text(.autoFoldReclaimWaiting))
        default:
            if isWorking {
                parts.append(L10n.text(.autoFoldFolding))
            }
        }
        if waitingKnown {
            parts.append(L10n.format(.autoFoldProgressFoldedFormat, managedCount, waitingCount))
        } else if managedCount > 0 {
            parts.append(L10n.format(.autoFoldProgressFoldedOnlyFormat, managedCount))
        } else if parts.isEmpty {
            parts.append(L10n.text(.autoFoldWaitingForCheck))
        }
        if !lastError.isEmpty {
            parts.append(L10n.text(.autoFoldRetry))
        }
        // The next check only means something once this pass has stopped.
        if let nextCheckAt, !isWorking {
            parts.append(Self.relativeCheck(from: nextCheckAt, now: now))
        }
        return parts.joined(separator: " · ")
    }

    static func relativeCheck(from date: Date, now: Date) -> String {
        let seconds = date.timeIntervalSince(now)
        if seconds <= 0 {
            return L10n.text(.autoFoldCheckSoon)
        }
        if seconds < 90 {
            return L10n.text(.autoFoldCheckUnderTwoMinutes)
        }
        if seconds < 3600 {
            return L10n.format(.autoFoldCheckMinutesFormat, Int((seconds / 60).rounded()))
        }
        let hours = max(1, Int((seconds / 3600).rounded()))
        return L10n.format(.autoFoldCheckHoursFormat, hours)
    }

    static func load(from url: URL) -> EnrollmentProgress? {
        guard let data = try? DurableAppGroupFile.read(url) else { return nil }
        guard let file = try? JSONDecoder().decode(EnrollmentProgressFile.self, from: data) else {
            return nil
        }
        guard file.version == 1 else { return nil }
        return EnrollmentProgress(
            storePath: file.storePath,
            enabled: file.enabled,
            interval: file.interval,
            stableFor: file.stableFor,
            archivedOnly: file.archivedOnly,
            phase: file.phase,
            managedCount: file.managedCount,
            waitingCount: file.waitingCount,
            waitingKnown: file.waitingKnown,
            cycleTotal: file.cycleTotal,
            cycleDone: file.cycleDone,
            nextCheckAt: parseEnrollmentTime(file.nextCheckAt),
            lastError: file.lastError,
            errorKind: file.errorKind,
            updatedAt: parseEnrollmentTime(file.updatedAt)
        )
    }
}

private struct EnrollmentProgressFile: Decodable {
    var version: Int
    var storePath: String
    var enabled: Bool
    var interval: String
    var stableFor: String
    var archivedOnly: Bool
    var phase: String
    var managedCount: Int
    var waitingCount: Int
    var waitingKnown: Bool
    var cycleTotal: Int
    var cycleDone: Int
    var nextCheckAt: String
    var lastError: String
    var errorKind: String
    var updatedAt: String

    enum CodingKeys: String, CodingKey {
        case version
        case storePath = "store_path"
        case enabled
        case interval
        case stableFor = "stable_for"
        case archivedOnly = "archived_only"
        case phase
        case managedCount = "managed_count"
        case waitingCount = "waiting_count"
        case waitingKnown = "waiting_known"
        case cycleTotal = "cycle_total"
        case cycleDone = "cycle_done"
        case nextCheckAt = "next_check_at"
        case lastError = "last_error"
        case errorKind = "error_kind"
        case updatedAt = "updated_at"
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        version = try container.decode(Int.self, forKey: .version)
        storePath = try container.decodeIfPresent(String.self, forKey: .storePath) ?? ""
        enabled = try container.decodeIfPresent(Bool.self, forKey: .enabled) ?? false
        interval = try container.decodeIfPresent(String.self, forKey: .interval) ?? ""
        stableFor = try container.decodeIfPresent(String.self, forKey: .stableFor) ?? ""
        archivedOnly = try container.decodeIfPresent(Bool.self, forKey: .archivedOnly) ?? true
        phase = try container.decodeIfPresent(String.self, forKey: .phase) ?? ""
        managedCount = try container.decodeIfPresent(Int.self, forKey: .managedCount) ?? 0
        waitingCount = try container.decodeIfPresent(Int.self, forKey: .waitingCount) ?? 0
        waitingKnown = try container.decodeIfPresent(Bool.self, forKey: .waitingKnown) ?? false
        cycleTotal = try container.decodeIfPresent(Int.self, forKey: .cycleTotal) ?? 0
        cycleDone = try container.decodeIfPresent(Int.self, forKey: .cycleDone) ?? 0
        nextCheckAt = try container.decodeIfPresent(String.self, forKey: .nextCheckAt) ?? ""
        lastError = try container.decodeIfPresent(String.self, forKey: .lastError) ?? ""
        errorKind = try container.decodeIfPresent(String.self, forKey: .errorKind) ?? ""
        updatedAt = try container.decodeIfPresent(String.self, forKey: .updatedAt) ?? ""
    }
}

func parseEnrollmentTime(_ raw: String) -> Date? {
    guard !raw.isEmpty else { return nil }
    let fractional = ISO8601DateFormatter()
    fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let date = fractional.date(from: raw) {
        return date
    }
    let plain = ISO8601DateFormatter()
    plain.formatOptions = [.withInternetDateTime]
    return plain.date(from: raw)
}
