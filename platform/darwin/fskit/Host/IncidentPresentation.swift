import AppKit
import Darwin
import Foundation
import OSLog
import SwiftUI

@_silgen_name("flock")
private func codexfold_flock(_ descriptor: Int32, _ operation: Int32) -> Int32

struct IncidentAcknowledgement: Codable, Equatable {
    let incidentID: String
    let incidentSince: TimeInterval
    let reason: String
    let impact: String
    let syntheticScope: String?

    init(incident: FrontendIncident) {
        incidentID = incident.id
        incidentSince = incident.since.timeIntervalSince1970
        reason = incident.reason
        impact = incident.impact
        syntheticScope = Self.syntheticOccurrenceID(for: incident.id)
    }

    func matches(_ incident: FrontendIncident) -> Bool {
        let candidate = IncidentAcknowledgement(incident: incident)
        if incidentID == candidate.incidentID && incidentSince == candidate.incidentSince {
            return true
        }
        guard let syntheticScope, let candidateOccurrence = candidate.syntheticScope else { return false }
        return syntheticScope == candidateOccurrence
    }

    private static func syntheticOccurrenceID(for incidentID: String) -> String? {
        for source in [
            "daemon-status-channel-",
            "managed-status-channel-",
            "supervisor-status-channel-",
            "supervisor:",
        ] where incidentID.hasPrefix(source) {
            return "synthetic:\(incidentID)"
        }
        return nil
    }
}

struct IncidentAcknowledgementStore {
    let url: URL

    init(appGroupURL: URL) {
        url = appGroupURL.appendingPathComponent("incident-acknowledgement.json", isDirectory: false)
    }

    func contains(_ incident: FrontendIncident) -> Bool {
        guard let data = try? DurableAppGroupFile.read(url),
              let acknowledgement = try? JSONDecoder().decode(IncidentAcknowledgement.self, from: data) else {
            return false
        }
        return acknowledgement.matches(incident)
    }

    func acknowledge(_ incident: FrontendIncident) throws {
        let data = try JSONEncoder().encode(IncidentAcknowledgement(incident: incident))
        try DurableAppGroupFile.write(data, to: url)
    }

    func clear(matching incident: FrontendIncident) {
        guard contains(incident) else { return }
        try? DurableAppGroupFile.remove(url)
    }

    func clearAll() {
        try? DurableAppGroupFile.remove(url)
    }
}

final class IncidentPresentationLease {
    private let lockURL: URL
    private var descriptor: Int32 = -1
    private(set) var isHeld = false

    init(appGroupURL: URL) {
        lockURL = appGroupURL.appendingPathComponent("incident-presenter.lock", isDirectory: false)
    }

    deinit {
        release()
    }

    func acquire() -> Bool {
        if isHeld { return true }
        if descriptor < 0 {
            descriptor = Darwin.open(lockURL.path, O_CREAT | O_RDWR | O_CLOEXEC, S_IRUSR | S_IWUSR)
            guard descriptor >= 0 else { return false }
        }
        guard codexfold_flock(descriptor, LOCK_EX | LOCK_NB) == 0 else { return false }
        isHeld = true
        return true
    }

    func release() {
        guard descriptor >= 0 else { return }
        if isHeld {
            _ = codexfold_flock(descriptor, LOCK_UN)
        }
        _ = Darwin.close(descriptor)
        descriptor = -1
        isHeld = false
    }
}

@MainActor
final class IncidentWindowPresenter: NSObject, NSWindowDelegate {
    private static let logger = Logger(
        subsystem: "vip.jstar.codexfold.fskitprofileprobe",
        category: "incident-presentation"
    )

    private let acknowledgementStore: IncidentAcknowledgementStore
    private let diagnosticPayload: (FrontendIncident) -> Data?
    private let presentWindow: ((NSWindowController) -> Void)?
    private let localAuthorizationCapability = LocalAuthorizationCapability.current()
    private var incidentController: NSWindowController?
    private var presentedIncident: FrontendIncident?

    init(
        appGroupURL: URL,
        diagnosticPayload: @escaping (FrontendIncident) -> Data?,
        presentWindow: ((NSWindowController) -> Void)? = nil
    ) {
        acknowledgementStore = IncidentAcknowledgementStore(appGroupURL: appGroupURL)
        self.diagnosticPayload = diagnosticPayload
        self.presentWindow = presentWindow
    }

    func handle(_ update: IncidentUpdate) {
        switch update {
        case .active(let incident, let isNew):
            present(incidentForPresentation(incident), isNew: isNew)
        case .recovered(let incident):
            let presented = incidentForPresentation(incident)
            acknowledgementStore.clear(matching: presented)
            presentedIncident = presented
            incidentController?.close()
        }
    }

    func presentCurrent(_ incident: FrontendIncident) {
        present(incidentForPresentation(incident), isNew: true)
    }

    func clearAcknowledgementAfterHealthyBaseline() {
        acknowledgementStore.clearAll()
    }

    func windowWillClose(_ notification: Notification) {
        guard notification.object as AnyObject? === incidentController?.window else { return }
        if let incident = presentedIncident {
            if incident.recoveredAt == nil {
                do {
                    try acknowledgementStore.acknowledge(incident)
                } catch {
                    Self.logger.error("Unable to persist incident acknowledgement: \(String(describing: error), privacy: .private)")
                }
            } else {
                acknowledgementStore.clear(matching: incident)
            }
        }
        incidentController = nil
        presentedIncident = nil
    }

    private func present(_ incident: FrontendIncident, isNew: Bool) {
        if let incidentController {
            presentedIncident = incident
            updateWindow(incidentController, incident: incident)
            if isNew {
                show(incidentController)
            }
            return
        }
        guard isNew, !acknowledgementStore.contains(incident) else { return }
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 500, height: 390),
            styleMask: [.titled, .closable],
            backing: .buffered,
            defer: false
        )
        window.title = L10n.text(.productName)
        window.center()
        window.level = .floating
        window.isReleasedWhenClosed = false
        window.delegate = self
        let controller = NSWindowController(window: window)
        incidentController = controller
        presentedIncident = incident
        updateWindow(controller, incident: incident)
        show(controller)
    }

    private func show(_ controller: NSWindowController) {
        if let presentWindow {
            presentWindow(controller)
            return
        }
        controller.showWindow(nil)
        controller.window?.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
        NSApp.requestUserAttention(.criticalRequest)
    }

    private func updateWindow(_ controller: NSWindowController, incident: FrontendIncident) {
        controller.window?.contentViewController = NSHostingController(
            rootView: IncidentView(
                incident: incident,
                exportDiagnostics: { [weak self] in self?.exportDiagnostics(incident) },
                close: { [weak controller] in controller?.close() }
            )
        )
        controller.window?.title = L10n.text(.productName)
    }

    private func incidentForPresentation(_ incident: FrontendIncident) -> FrontendIncident {
        let authorizationDetail: String
        switch localAuthorizationCapability.administratorMembership {
        case .member:
            authorizationDetail = L10n.text(.administratorCapabilityMemberDetail)
        case .notMember:
            authorizationDetail = L10n.text(.administratorCapabilityNotMemberDetail)
        case .unknown:
            authorizationDetail = L10n.text(.administratorCapabilityUnknownDetail)
        }
        let technicalDetails = incident.technicalDetails.isEmpty
            ? authorizationDetail
            : "\(incident.technicalDetails)\n\n\(authorizationDetail)"
        return FrontendIncident(
            id: incident.id,
            since: incident.since,
            reason: incident.reason,
            impact: incident.impact,
            recommendations: incident.recommendations,
            technicalDetails: technicalDetails,
            recoveredAt: incident.recoveredAt
        )
    }

    private func exportDiagnostics(_ incident: FrontendIncident) {
        guard let data = diagnosticPayload(incident) else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "codexfold-diagnostics-\(Self.filenameTimestamp.string(from: Date())).json"
        panel.allowedContentTypes = [.json]
        panel.canCreateDirectories = true
        panel.begin { response in
            guard response == .OK, let url = panel.url else { return }
            do {
                try data.write(to: url, options: .atomic)
            } catch {
                let alert = NSAlert(error: error)
                alert.messageText = L10n.text(.diagnosticsSaveFailed)
                alert.runModal()
            }
        }
    }

    private static let filenameTimestamp: DateFormatter = {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        return formatter
    }()
}

@MainActor
final class IncidentPresentationCoordinator {
    private let statusStore: StatusStore
    private let lease: IncidentPresentationLease
    private let presenter: IncidentWindowPresenter
    private var timer: Timer?

    init(appGroupURL: URL, statusStore: StatusStore) {
        self.statusStore = statusStore
        lease = IncidentPresentationLease(appGroupURL: appGroupURL)
        presenter = IncidentWindowPresenter(
            appGroupURL: appGroupURL,
            diagnosticPayload: { [weak statusStore] incident in
                statusStore?.diagnosticPayload(incident: incident)
            }
        )
    }

    func start() {
        statusStore.onIncident = { [weak self] update in
            guard let self, self.lease.isHeld else { return }
            self.presenter.handle(update)
        }
        attemptLease()
        let timer = Timer(timeInterval: 1, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.attemptLease() }
        }
        RunLoop.main.add(timer, forMode: .common)
        self.timer = timer
    }

    func stop() {
        timer?.invalidate()
        timer = nil
        statusStore.onIncident = nil
        lease.release()
    }

    private func attemptLease() {
        if statusStore.hasTrustedHealthyBaseline {
            presenter.clearAcknowledgementAfterHealthyBaseline()
        }
        let alreadyHeld = lease.isHeld
        guard lease.acquire(), !alreadyHeld else { return }
        if let incident = statusStore.currentIncident {
            presenter.presentCurrent(incident)
        }
    }
}
