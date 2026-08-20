import Charts
import SwiftUI

struct StatusPopoverView: View {
    @ObservedObject var store: StatusStore
    let openDashboard: () -> Void
    let exportDiagnostics: () -> Void
    let copyDiagnostics: () -> Void
    let revealStatusFiles: () -> Void
    let quit: () -> Void

    private var compactComponents: [ComponentStatus] {
        store.components.filter {
            !["storage", "store", "activity"].contains($0.id.lowercased())
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            VStack(alignment: .leading, spacing: 13) {
                StatusHeader(
                    health: store.overallHealth,
                    summary: store.isRuntimeUnconnected ? "" : store.summary
                )

                if let metrics = store.storageMetrics {
                    CompactStorageOverview(
                        metrics: metrics,
                        activity: store.currentActivity,
                        samples: store.storageHistory
                    )
                    .padding(13)
                    .background(
                        Color.primary.opacity(0.035),
                        in: RoundedRectangle(cornerRadius: 14, style: .continuous)
                    )
                    .overlay {
                        RoundedRectangle(cornerRadius: 14, style: .continuous)
                            .stroke(Color.primary.opacity(0.065), lineWidth: 1)
                    }
                } else if store.isRuntimeUnconnected {
                    UnconnectedStateCard()
                }

                if !store.isRuntimeUnconnected {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(L10n.text(.components))
                            .font(.caption.weight(.semibold))
                            .foregroundStyle(.secondary)

                        if compactComponents.isEmpty {
                            Label(L10n.text(.noComponents), systemImage: "hourglass")
                                .font(.callout)
                                .foregroundStyle(.secondary)
                                .frame(maxWidth: .infinity, alignment: .leading)
                                .padding(11)
                        } else {
                            VStack(spacing: 0) {
                                ForEach(Array(compactComponents.prefix(4).enumerated()), id: \.element.id) { index, component in
                                    CompactComponentRow(component: component)
                                    if index < min(compactComponents.count, 4) - 1 {
                                        Divider().padding(.leading, 27)
                                    }
                                }
                            }
                            .background(
                                Color.primary.opacity(0.025),
                                in: RoundedRectangle(cornerRadius: 12, style: .continuous)
                            )
                        }
                    }
                }

                if let notice = store.hostNotice {
                    Label(notice, systemImage: "info.circle")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .padding(11)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .background(
                            Color.accentColor.opacity(0.07),
                            in: RoundedRectangle(cornerRadius: 11, style: .continuous)
                        )
                }
            }
            .padding(16)

            Spacer(minLength: 0)
            Divider()

            HStack(spacing: 10) {
                Button(action: openDashboard) {
                    Label(L10n.text(.openDashboard), systemImage: "macwindow")
                        .frame(maxWidth: .infinity)
                }
                .buttonStyle(.borderedProminent)

                Button(action: store.refresh) {
                    Image(systemName: "arrow.clockwise")
                }
                .buttonStyle(.bordered)
                .help(L10n.text(.checkStatus))
                .accessibilityLabel(L10n.text(.checkStatus))

                Menu {
                    Button(action: exportDiagnostics) {
                        Label(L10n.text(.exportDiagnostics), systemImage: "square.and.arrow.up")
                    }
                    Button(action: copyDiagnostics) {
                        Label(L10n.text(.copyDiagnostics), systemImage: "doc.on.doc")
                    }
                    Button(action: revealStatusFiles) {
                        Label(L10n.text(.revealStatusFiles), systemImage: "folder")
                    }
                    Divider()
                    Button(action: quit) {
                        Label(L10n.text(.quit), systemImage: "xmark.circle")
                    }
                } label: {
                    Image(systemName: "ellipsis")
                        .frame(width: 16)
                }
                .menuStyle(.borderlessButton)
                .menuIndicator(.hidden)
                .fixedSize()
                .accessibilityLabel(L10n.text(.technicalDetails))
            }
            .padding(12)
        }
        .frame(width: 360)
        .frame(maxHeight: .infinity, alignment: .top)
    }
}

struct DashboardView: View {
    @ObservedObject var store: StatusStore
    let exportDiagnostics: () -> Void
    let copyDiagnostics: () -> Void
    let revealStatusFiles: () -> Void
    @State private var historyRange: HistoryRange = .day

    private var runtimeComponents: [ComponentStatus] {
        store.components.filter {
            !["activity", "storage", "store"].contains($0.id.lowercased())
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .center, spacing: 16) {
                StatusHeader(health: store.overallHealth, summary: store.summary)
                Spacer()
                Button(action: store.refresh) {
                    Label(L10n.text(.checkStatus), systemImage: "arrow.clockwise")
                }
                Button(action: exportDiagnostics) {
                    Label(L10n.text(.exportDiagnostics), systemImage: "square.and.arrow.up")
                }
                Menu {
                    Button(action: copyDiagnostics) {
                        Label(L10n.text(.copyDiagnostics), systemImage: "doc.on.doc")
                    }
                    Button(action: revealStatusFiles) {
                        Label(L10n.text(.revealStatusFiles), systemImage: "folder")
                    }
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
                .menuStyle(.borderlessButton)
                .help(L10n.text(.technicalDetails))
            }
            .padding(20)

            Divider()

            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    if let metrics = store.storageMetrics {
                        StorageOverview(metrics: metrics)
                    }

                    StorageHistorySection(
                        samples: store.storageHistory,
                        range: $historyRange
                    )

                    ActivityHistorySection(
                        samples: store.storageHistory,
                        current: store.currentActivity,
                        range: historyRange
                    )

                    IncidentHistorySection(
                        incidents: store.incidentHistory,
                        range: historyRange
                    )

                    VStack(alignment: .leading, spacing: 12) {
                        Text(L10n.text(.components))
                            .font(.title3.weight(.semibold))
                        if runtimeComponents.isEmpty {
                            ContentUnavailableView(
                                L10n.text(.statusUnknown),
                                systemImage: "waveform.path.ecg",
                                description: Text(L10n.text(.waitingForStatus))
                            )
                            .frame(maxWidth: .infinity, minHeight: 180)
                        } else {
                            VStack(spacing: 0) {
                                ForEach(Array(runtimeComponents.enumerated()), id: \.element.id) { index, component in
                                    ComponentRow(component: component)
                                    if index < runtimeComponents.count - 1 { Divider() }
                                }
                            }
                        }
                    }
                    .padding(18)
                    .background(Color.primary.opacity(0.018), in: RoundedRectangle(cornerRadius: 14))
                    .overlay {
                        RoundedRectangle(cornerRadius: 14)
                            .stroke(Color.primary.opacity(0.06), lineWidth: 1)
                    }

                    if let notice = store.hostNotice {
                        Label(notice, systemImage: "info.circle")
                            .font(.callout)
                            .foregroundStyle(.secondary)
                    }

                    HStack {
                        Spacer()
                        if let updated = store.lastReadAt {
                            Text(L10n.format(.lastUpdatedFormat, updated.formatted(date: .omitted, time: .standard)))
                                .font(.caption)
                                .foregroundStyle(.tertiary)
                        }
                    }
                }
                .padding(20)
            }
        }
        .frame(minWidth: 720, minHeight: 620)
    }
}

struct IncidentView: View {
    let incident: FrontendIncident
    let exportDiagnostics: () -> Void
    let close: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    HStack(alignment: .center, spacing: 13) {
                        ZStack {
                            Circle()
                                .fill(incidentTint.opacity(0.11))
                                .frame(width: 38, height: 38)
                            Image(systemName: incident.recoveredAt == nil ? "exclamationmark" : "checkmark")
                                .font(.system(size: 16, weight: .bold))
                                .foregroundStyle(incidentTint)
                        }
                        .accessibilityHidden(true)

                        VStack(alignment: .leading, spacing: 3) {
                            Text(incident.recoveredAt == nil ? L10n.text(.incidentTitle) : L10n.text(.incidentRecoveredTitle))
                                .font(.title3.weight(.semibold))
                            Text(incident.recoveredAt == nil
                                 ? incident.since.formatted(date: .abbreviated, time: .shortened)
                                 : L10n.text(.incidentRecoveredBody))
                                .font(.callout)
                                .foregroundStyle(.secondary)
                        }
                    }

                    HStack(alignment: .top, spacing: 10) {
                        Image(systemName: incident.recoveredAt == nil ? "shield.lefthalf.filled" : "checkmark.shield.fill")
                            .foregroundStyle(incidentTint)
                            .padding(.top, 2)
                        Text(incident.impact)
                            .font(.callout.weight(.medium))
                            .fixedSize(horizontal: false, vertical: true)
                            .textSelection(.enabled)
                    }
                    .padding(12)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(
                        Color.primary.opacity(0.035),
                        in: RoundedRectangle(cornerRadius: 12, style: .continuous)
                    )
                    .overlay {
                        RoundedRectangle(cornerRadius: 12, style: .continuous)
                            .stroke(incidentTint.opacity(0.18), lineWidth: 1)
                    }

                    IncidentSection(title: L10n.text(.reason), message: incident.reason)

                    if !incident.recommendations.isEmpty {
                        VStack(alignment: .leading, spacing: 9) {
                            Text(L10n.text(.recommendedActions))
                                .font(.subheadline.weight(.semibold))
                            ForEach(Array(incident.recommendations.enumerated()), id: \.offset) { _, recommendation in
                                HStack(alignment: .top, spacing: 9) {
                                    Image(systemName: "arrow.right")
                                        .font(.caption)
                                        .foregroundStyle(incidentTint)
                                        .padding(.top, 3)
                                        .accessibilityHidden(true)
                                    Text(recommendation)
                                        .font(.callout)
                                        .textSelection(.enabled)
                                        .fixedSize(horizontal: false, vertical: true)
                                }
                            }
                        }
                    }

                    DisclosureGroup(L10n.text(.technicalDetails)) {
                        Text(incident.technicalDetails)
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.top, 6)
                    }
                    .font(.callout)
                }
                .padding(18)
            }

            Divider()

            HStack {
                Button(action: exportDiagnostics) {
                    Label(L10n.text(.exportDiagnostics), systemImage: "square.and.arrow.up")
                }
                .buttonStyle(.borderless)
                Spacer()
                Button(L10n.text(.close), action: close)
                    .buttonStyle(.borderedProminent)
                    .keyboardShortcut(.defaultAction)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 12)
            .background(Color.primary.opacity(0.025))
        }
        .frame(width: 500, height: 390)
    }

    private var incidentTint: Color {
        incident.recoveredAt == nil ? .orange : .green
    }
}

private enum HistoryRange: String, CaseIterable, Identifiable {
    case hour
    case day
    case week
    case month

    var id: String { rawValue }

    var interval: TimeInterval {
        switch self {
        case .hour: return 60 * 60
        case .day: return 24 * 60 * 60
        case .week: return 7 * 24 * 60 * 60
        case .month: return 30 * 24 * 60 * 60
        }
    }

    var title: String {
        switch self {
        case .hour: return L10n.text(.rangeOneHour)
        case .day: return L10n.text(.rangeOneDay)
        case .week: return L10n.text(.rangeSevenDays)
        case .month: return L10n.text(.rangeThirtyDays)
        }
    }
}

private struct UnconnectedStateCard: View {
    var body: some View {
        HStack(alignment: .top, spacing: 11) {
            ZStack {
                Circle()
                    .fill(Color.secondary.opacity(0.11))
                    .frame(width: 36, height: 36)
                Image(systemName: "bolt.horizontal.circle")
                    .font(.system(size: 16, weight: .medium))
                    .foregroundStyle(.secondary)
            }
            .accessibilityHidden(true)

            VStack(alignment: .leading, spacing: 4) {
                Text(L10n.text(.unconnectedTitle))
                    .font(.subheadline.weight(.semibold))
                Text(L10n.text(.unconnectedDetail))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(13)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(
            Color.primary.opacity(0.025),
            in: RoundedRectangle(cornerRadius: 13, style: .continuous)
        )
        .overlay {
            RoundedRectangle(cornerRadius: 13, style: .continuous)
                .stroke(Color.primary.opacity(0.06), lineWidth: 1)
        }
    }
}

private struct CompactStorageOverview: View {
    let metrics: StorageMetrics
    let activity: IOActivity?
    let samples: [StorageHistorySample]

    private var recentSamples: [StorageHistorySample] {
        filteredStorageSamples(samples, range: .day, now: Date())
    }

    private var savedYDomain: ClosedRange<Double> {
        let values = recentSamples.map { Double(max(0, $0.logicalBytes - $0.physicalBytes)) }
        guard let minimum = values.min(), let maximum = values.max() else { return 0...1 }
        let padding = max((maximum - minimum) * 0.35, maximum * 0.02, 1)
        return max(0, minimum - padding)...(maximum + padding)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 9) {
            HStack(alignment: .firstTextBaseline) {
                VStack(alignment: .leading, spacing: 2) {
                    Text(L10n.text(.spaceSaved))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(formattedBytes(metrics.savedBytes))
                        .font(.title2.weight(.semibold).monospacedDigit())
                }
                Spacer()
                Text("\(Int((metrics.savingsFraction * 100).rounded()))%")
                    .font(.title3.weight(.semibold).monospacedDigit())
                    .foregroundStyle(.green)
            }

            if !recentSamples.isEmpty {
                Chart(recentSamples) { sample in
                    let saved = Double(max(0, sample.logicalBytes - sample.physicalBytes))
                    AreaMark(
                        x: .value("Time", sample.capturedAt),
                        yStart: .value("Baseline", savedYDomain.lowerBound),
                        yEnd: .value("Space saved", saved)
                    )
                    .foregroundStyle(Color.green.opacity(0.09))
                    .interpolationMethod(.monotone)
                    LineMark(
                        x: .value("Time", sample.capturedAt),
                        y: .value("Space saved", saved)
                    )
                    .foregroundStyle(Color.green)
                    .lineStyle(StrokeStyle(lineWidth: 2))
                    .interpolationMethod(.monotone)
                    if recentSamples.count == 1 {
                        PointMark(
                            x: .value("Time", sample.capturedAt),
                            y: .value("Space saved", saved)
                        )
                        .foregroundStyle(Color.green)
                    }
                }
                .chartYScale(domain: savedYDomain)
                .chartXAxis(.hidden)
                .chartYAxis(.hidden)
                .frame(height: 40)
            }

            HStack {
                CompactValueLabel(
                    title: L10n.text(.originalSize),
                    value: formattedBytes(metrics.logicalBytes),
                    systemImage: "doc.on.doc"
                )
                Spacer()
                CompactValueLabel(
                    title: L10n.text(.currentUsage),
                    value: formattedBytes(metrics.physicalBytes),
                    systemImage: "internaldrive",
                    alignment: .trailing
                )
            }

            if let activity {
                HStack {
                    CompactValueLabel(
                        title: L10n.text(.readRate),
                        value: formattedRate(activity.readBytesPerSecond),
                        systemImage: "arrow.down"
                    )
                    Spacer()
                    CompactValueLabel(
                        title: L10n.text(.writeRate),
                        value: formattedRate(activity.writtenBytesPerSecond),
                        systemImage: "arrow.up",
                        alignment: .trailing
                    )
                }
            }
        }
    }
}

private struct CompactValueLabel: View {
    let title: String
    let value: String
    let systemImage: String
    var alignment: HorizontalAlignment = .leading

    var body: some View {
        VStack(alignment: alignment, spacing: 2) {
            Label(title, systemImage: systemImage)
                .font(.caption2)
                .foregroundStyle(.tertiary)
            Text(value)
                .font(.caption.monospacedDigit())
                .foregroundStyle(.secondary)
        }
    }
}

private struct StorageOverview: View {
    let metrics: StorageMetrics

    var body: some View {
        LazyVGrid(
            columns: Array(repeating: GridItem(.flexible(), spacing: 12), count: 4),
            spacing: 12
        ) {
            StorageMetricTile(
                title: L10n.text(.spaceSaved),
                value: formattedBytes(metrics.savedBytes),
                systemImage: "arrow.down.right.circle.fill",
                tint: .green
            )
            StorageMetricTile(
                title: L10n.text(.savingsRate),
                value: "\(Int((metrics.savingsFraction * 100).rounded()))%",
                systemImage: "percent",
                tint: .green
            )
            StorageMetricTile(
                title: L10n.text(.sessionDataSize),
                value: formattedBytes(metrics.logicalBytes),
                systemImage: "doc.on.doc",
                footnote: metrics.managedSessions.map { "\(L10n.text(.managedSessions)): \($0)" },
                tint: .primary
            )
            StorageMetricTile(
                title: L10n.text(.diskUsage),
                value: formattedBytes(metrics.physicalBytes),
                systemImage: "internaldrive"
            )
        }
    }
}

private struct StorageMetricTile: View {
    let title: String
    let value: String
    let systemImage: String
    var footnote: String?
    var tint: Color = .accentColor

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            Label(title, systemImage: systemImage)
                .font(.caption.weight(.medium))
                .foregroundStyle(.secondary)
            Text(value)
                .font(.title2.weight(.semibold).monospacedDigit())
                .foregroundStyle(tint)
                .lineLimit(1)
                .minimumScaleFactor(0.72)
            if let footnote {
                Text(footnote)
                    .font(.caption2)
                    .foregroundStyle(.tertiary)
                    .lineLimit(1)
            }
        }
        .frame(maxWidth: .infinity, minHeight: 70, alignment: .leading)
        .padding(12)
        .background(Color.primary.opacity(0.025), in: RoundedRectangle(cornerRadius: 12))
        .overlay {
            RoundedRectangle(cornerRadius: 12)
                .stroke(Color.primary.opacity(0.06), lineWidth: 1)
        }
    }
}

private struct StorageHistorySection: View {
    let samples: [StorageHistorySample]
    @Binding var range: HistoryRange

    private var visibleSamples: [StorageHistorySample] {
        filteredStorageSamples(samples, range: range, now: Date())
    }

    private var yMaximum: Double {
        max(1, visibleSamples.reduce(0) {
            max($0, Double(max($1.logicalBytes, $1.physicalBytes)))
        } * 1.08)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack {
                Text(L10n.text(.storageHistory))
                    .font(.title3.weight(.semibold))
                Spacer()
                Picker("", selection: $range) {
                    ForEach(HistoryRange.allCases) { value in
                        Text(value.title).tag(value)
                    }
                }
                .labelsHidden()
                .pickerStyle(.segmented)
                .frame(width: 330)
            }

            if visibleSamples.isEmpty {
                ContentUnavailableView(
                    L10n.text(.storageHistory),
                    systemImage: "chart.xyaxis.line",
                    description: Text(L10n.text(.noStorageHistory))
                )
                .frame(maxWidth: .infinity, minHeight: 220)
            } else {
                HStack(spacing: 18) {
                    StorageLegend(color: .secondary, title: L10n.text(.originalSize))
                    StorageLegend(color: .accentColor, title: L10n.text(.currentUsage))
                    StorageLegend(color: .green.opacity(0.6), title: L10n.text(.spaceSaved))
                }
                .font(.caption)

                Chart(visibleSamples) { sample in
                    if sample.logicalBytes >= sample.physicalBytes {
                        AreaMark(
                            x: .value("Time", sample.capturedAt),
                            yStart: .value("Disk usage", Double(sample.physicalBytes)),
                            yEnd: .value("Original size", Double(sample.logicalBytes))
                        )
                        .foregroundStyle(Color.green.opacity(0.08))
                        .interpolationMethod(.monotone)
                    }
                    LineMark(
                        x: .value("Time", sample.capturedAt),
                        y: .value("Original size", Double(sample.logicalBytes)),
                        series: .value("Series", "original")
                    )
                    .foregroundStyle(Color.secondary)
                    .lineStyle(StrokeStyle(lineWidth: 1.5, dash: [4, 3]))
                    .interpolationMethod(.monotone)
                    LineMark(
                        x: .value("Time", sample.capturedAt),
                        y: .value("Disk usage", Double(sample.physicalBytes)),
                        series: .value("Series", "physical")
                    )
                    .foregroundStyle(Color.accentColor)
                    .lineStyle(StrokeStyle(lineWidth: 2.25))
                    .interpolationMethod(.monotone)
                    if visibleSamples.count == 1 {
                        PointMark(
                            x: .value("Time", sample.capturedAt),
                            y: .value("Disk usage", Double(sample.physicalBytes))
                        )
                        .foregroundStyle(Color.accentColor)
                    }
                }
                .chartYScale(domain: 0...yMaximum)
                .chartYAxis {
                    AxisMarks(position: .leading, values: .automatic(desiredCount: 5)) { value in
                        AxisGridLine()
                        AxisTick()
                        AxisValueLabel {
                            if let bytes = value.as(Double.self) {
                                Text(formattedBytes(Int64(bytes)))
                            }
                        }
                    }
                }
                .chartXAxis {
                    AxisMarks(values: .automatic(desiredCount: 6))
                }
                .frame(height: 240)
            }
        }
        .padding(16)
        .background(Color.primary.opacity(0.018), in: RoundedRectangle(cornerRadius: 14))
        .overlay {
            RoundedRectangle(cornerRadius: 14)
                .stroke(Color.primary.opacity(0.06), lineWidth: 1)
        }
    }
}

private struct StorageLegend: View {
    let color: Color
    let title: String

    var body: some View {
        HStack(spacing: 6) {
            Capsule().fill(color).frame(width: 18, height: 3)
            Text(title).foregroundStyle(.secondary)
        }
    }
}

private struct ActivityHistorySection: View {
    let samples: [StorageHistorySample]
    let current: IOActivity?
    let range: HistoryRange

    private var visibleSamples: [StorageHistorySample] {
        filteredStorageSamples(samples, range: range, now: Date()).filter {
            $0.readBytesPerSecond != nil || $0.writtenBytesPerSecond != nil
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HStack(alignment: .firstTextBaseline) {
                Text(L10n.text(.ioActivity))
                    .font(.title3.weight(.semibold))
                Spacer()
                if let current {
                    ActivityRateLabel(
                        title: L10n.text(.readRate),
                        value: current.readBytesPerSecond,
                        color: .accentColor,
                        systemImage: "arrow.down"
                    )
                    ActivityRateLabel(
                        title: L10n.text(.writeRate),
                        value: current.writtenBytesPerSecond,
                        color: .orange,
                        systemImage: "arrow.up"
                    )
                }
            }

            if visibleSamples.isEmpty {
                ContentUnavailableView(
                    L10n.text(.ioActivity),
                    systemImage: "waveform.path.ecg",
                    description: Text(L10n.text(.noActivityHistory))
                )
                .frame(maxWidth: .infinity, minHeight: 170)
            } else {
                HStack(spacing: 18) {
                    StorageLegend(color: .accentColor, title: L10n.text(.readRate))
                    StorageLegend(color: .orange, title: L10n.text(.writeRate))
                }
                .font(.caption)

                Chart(visibleSamples) { sample in
                    if let bytes = sample.readBytesPerSecond {
                        LineMark(
                            x: .value("Time", sample.capturedAt),
                            y: .value("Read", bytes),
                            series: .value("Series", "read")
                        )
                        .foregroundStyle(Color.accentColor)
                        .lineStyle(StrokeStyle(lineWidth: 2))
                        .interpolationMethod(.monotone)
                    }
                    if let bytes = sample.writtenBytesPerSecond {
                        LineMark(
                            x: .value("Time", sample.capturedAt),
                            y: .value("Write", bytes),
                            series: .value("Series", "write")
                        )
                        .foregroundStyle(Color.orange)
                        .lineStyle(StrokeStyle(lineWidth: 2))
                        .interpolationMethod(.monotone)
                    }
                }
                .chartYAxis {
                    AxisMarks(position: .leading, values: .automatic(desiredCount: 4)) { value in
                        AxisGridLine()
                        AxisTick()
                        AxisValueLabel {
                            if let bytes = value.as(Double.self) {
                                Text(formattedRate(bytes))
                            }
                        }
                    }
                }
                .chartXAxis {
                    AxisMarks(values: .automatic(desiredCount: 6))
                }
                .frame(height: 190)
            }
        }
        .padding(16)
        .background(Color.primary.opacity(0.018), in: RoundedRectangle(cornerRadius: 14))
        .overlay {
            RoundedRectangle(cornerRadius: 14)
                .stroke(Color.primary.opacity(0.06), lineWidth: 1)
        }
    }
}

private struct ActivityRateLabel: View {
    let title: String
    let value: Double
    let color: Color
    let systemImage: String

    var body: some View {
        HStack(spacing: 5) {
            Image(systemName: systemImage).foregroundStyle(color)
            Text(title).foregroundStyle(.secondary)
            Text(formattedRate(value)).font(.callout.monospacedDigit())
        }
    }
}

private struct IncidentHistorySection: View {
    let incidents: [IncidentHistoryEntry]
    let range: HistoryRange

    private var visibleIncidents: [IncidentHistoryEntry] {
        let cutoff = Date().addingTimeInterval(-range.interval)
        return incidents.filter { ($0.recoveredAt ?? Date()) >= cutoff }.prefix(8).map { $0 }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(L10n.text(.recentIncidents))
                .font(.title3.weight(.semibold))
            if visibleIncidents.isEmpty {
                Label(L10n.text(.noRecentIncidents), systemImage: "checkmark.circle")
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, minHeight: 54, alignment: .leading)
            } else {
                VStack(spacing: 0) {
                    ForEach(Array(visibleIncidents.enumerated()), id: \.element.id) { index, incident in
                        IncidentHistoryRow(incident: incident)
                        if index < visibleIncidents.count - 1 { Divider() }
                    }
                }
            }
        }
        .padding(16)
        .background(Color.primary.opacity(0.018), in: RoundedRectangle(cornerRadius: 14))
        .overlay {
            RoundedRectangle(cornerRadius: 14)
                .stroke(Color.primary.opacity(0.06), lineWidth: 1)
        }
    }
}

private struct IncidentHistoryRow: View {
    let incident: IncidentHistoryEntry

    var body: some View {
        HStack(spacing: 12) {
            Circle()
                .fill(incident.recoveredAt == nil ? Color.red : Color.green)
                .frame(width: 9, height: 9)
            VStack(alignment: .leading, spacing: 3) {
                Text(incident.recoveredAt == nil ? L10n.text(.incidentOngoing) : L10n.text(.incidentRecovered))
                    .font(.callout.weight(.medium))
                Text(incident.since.formatted(date: .abbreviated, time: .shortened))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text(formattedDuration(incident.duration(relativeTo: Date())))
                .font(.callout.monospacedDigit())
                .foregroundStyle(.secondary)
        }
        .padding(.vertical, 10)
    }
}

private func filteredStorageSamples(
    _ samples: [StorageHistorySample],
    range: HistoryRange,
    now: Date
) -> [StorageHistorySample] {
    let cutoff = now.addingTimeInterval(-range.interval)
    let ordered = samples.sorted { $0.capturedAt < $1.capturedAt }
    return ordered.filter { $0.capturedAt >= cutoff && $0.capturedAt <= now.addingTimeInterval(60) }
}

private func formattedBytes(_ bytes: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: max(0, bytes), countStyle: .file)
}

private func formattedRate(_ bytesPerSecond: Double) -> String {
    "\(formattedBytes(Int64(max(0, bytesPerSecond))))/s"
}

private func formattedDuration(_ duration: TimeInterval) -> String {
    let formatter = DateComponentsFormatter()
    formatter.allowedUnits = duration >= 60 * 60 ? [.hour, .minute] : [.minute, .second]
    formatter.unitsStyle = .abbreviated
    formatter.maximumUnitCount = 2
    return formatter.string(from: max(0, duration)) ?? "0s"
}

private struct StatusHeader: View {
    let health: ComponentHealth
    let summary: String

    var body: some View {
        HStack(spacing: 11) {
            ZStack {
                Circle()
                    .fill(color.opacity(0.13))
                    .frame(width: 36, height: 36)
                Image(systemName: symbol)
                    .font(.system(size: 17, weight: .semibold))
                    .foregroundStyle(color)
            }
            .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 2) {
                Text(L10n.text(.productName)).font(.headline)
                if !summary.isEmpty {
                    Text(summary).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
            }
        }
    }

    private var symbol: String {
        switch health {
        case .healthy: return "checkmark.circle.fill"
        case .recovering: return "arrow.triangle.2.circlepath.circle.fill"
        case .failed: return "exclamationmark.triangle.fill"
        case .unknown: return "questionmark.circle"
        }
    }

    private var color: Color {
        switch health {
        case .healthy: return .green
        case .recovering: return .orange
        case .failed: return .red
        case .unknown: return .secondary
        }
    }
}

private struct CompactComponentRow: View {
    let component: ComponentStatus

    var body: some View {
        HStack(spacing: 9) {
            Image(systemName: componentSymbol(component.health))
                .font(.caption)
                .foregroundStyle(componentColor(component.health))
                .frame(width: 16)
                .accessibilityHidden(true)
            Text(componentDisplayName(component.id)).lineLimit(1)
            Spacer()
            if component.health != .healthy {
                Text(healthText(component.health))
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
        }
        .accessibilityElement(children: .combine)
        .accessibilityValue(healthText(component.health))
        .padding(.horizontal, 11)
        .frame(height: 34)
    }
}

private struct ComponentRow: View {
    let component: ComponentStatus

    var body: some View {
        HStack(alignment: .top, spacing: 14) {
            Image(systemName: componentSymbol(component.health))
                .font(.system(size: 17))
                .foregroundStyle(componentColor(component.health))
                .frame(width: 24)
                .accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 5) {
                HStack {
                    Text(componentDisplayName(component.id)).font(.headline)
                    Spacer()
                    Text(healthText(component.health)).font(.callout).foregroundStyle(.secondary)
                }
                if !componentTechnicalStatus(component).isEmpty {
                    DisclosureGroup(L10n.text(.technicalDetails)) {
                        Text(componentTechnicalStatus(component))
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(.secondary)
                            .textSelection(.enabled)
                            .padding(.top, 4)
                    }
                    .font(.callout)
                }
                HStack(spacing: 18) {
                    if let count = component.managedSessions {
                        Label("\(L10n.text(.managedSessions)): \(count)", systemImage: "doc.text")
                    }
                    if let bytes = component.logicalBytes {
                        Label("\(L10n.text(.logicalBytes)): \(ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file))", systemImage: "doc.on.doc")
                    }
                    if let bytes = component.physicalBytes {
                        Label("\(L10n.text(.physicalBytes)): \(ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file))", systemImage: "internaldrive")
                    }
                }
                .font(.caption)
                .foregroundStyle(.tertiary)
            }
        }
        .padding(.vertical, 12)
    }
}

private struct IncidentSection: View {
    let title: String
    let message: String

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.subheadline.weight(.semibold))
            Text(message)
                .font(.callout)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
                .textSelection(.enabled)
        }
    }
}

private func componentColor(_ health: ComponentHealth) -> Color {
    switch health {
    case .healthy: return .green
    case .recovering: return .orange
    case .failed: return .red
    case .unknown: return .secondary
    }
}

private func componentSymbol(_ health: ComponentHealth) -> String {
    switch health {
    case .healthy: return "checkmark.circle.fill"
    case .recovering: return "arrow.triangle.2.circlepath.circle.fill"
    case .failed: return "exclamationmark.triangle.fill"
    case .unknown: return "questionmark.circle"
    }
}

private func healthText(_ health: ComponentHealth) -> String {
    switch health {
    case .healthy: return L10n.text(.statusHealthy)
    case .recovering: return L10n.text(.statusRecovering)
    case .failed: return L10n.text(.statusAttention)
    case .unknown: return L10n.text(.statusUnknown)
    }
}

private func componentDisplayName(_ identifier: String) -> String {
    switch identifier.lowercased() {
    case "frontend", "native-fskit", "fskit-frontend": return L10n.text(.frontend)
    case "managed": return L10n.text(.managed)
    case "supervisor", "supervisor-status-channel": return L10n.text(.supervisor)
    case "daemon", "service", "backend": return L10n.text(.daemon)
    case "storage", "store": return L10n.text(.storage)
    default: return identifier
    }
}

private func componentTechnicalStatus(_ component: ComponentStatus) -> String {
    var lines: [String] = []
    if !component.summary.isEmpty,
       component.summary != componentDisplayName(component.id) {
        lines.append(component.summary)
    }
    if !component.detail.isEmpty,
       component.detail != component.summary {
        lines.append(component.detail)
    }
    return lines.joined(separator: "\n")
}
