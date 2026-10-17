import AppKit
import Darwin
import Foundation
import OSLog
import SwiftUI
import UserNotifications

@MainActor
protocol IncidentNotificationSending: AnyObject {
    var onOpen: ((String) -> Void)? { get set }
    func post(_ incident: FrontendIncident)
    func remove(_ incidentID: String)
}

@MainActor
final class SystemIncidentNotificationSender: NSObject, IncidentNotificationSending, UNUserNotificationCenterDelegate {
    var onOpen: ((String) -> Void)?
    private let center = UNUserNotificationCenter.current()
    private var active = Set<String>()

    override init() {
        super.init()
        center.delegate = self
    }

    func post(_ incident: FrontendIncident) {
        active.insert(incident.id)
        center.getNotificationSettings { [weak self] settings in
            Task { @MainActor in
                guard let self, self.active.contains(incident.id) else { return }
                switch settings.authorizationStatus {
                case .notDetermined:
                    // Provisional authorization is quiet: do not replace an
                    // unwanted fault window with a permission dialog.
                    self.center.requestAuthorization(options: [.alert, .provisional]) { [weak self] granted, _ in
                        Task { @MainActor in
                            if granted { self?.submit(incident) }
                        }
                    }
                case .authorized, .provisional:
                    self.submit(incident)
                default:
                    break // Denied notifications never fall back to a modal window.
                }
            }
        }
    }

    private func submit(_ incident: FrontendIncident) {
        guard active.contains(incident.id) else { return }
        let content = UNMutableNotificationContent()
        content.title = L10n.text(.productName)
        content.subtitle = incident.reason
        content.body = String(incident.impact.prefix(280))
        content.sound = nil
        content.userInfo = ["codexfoldIncidentID": incident.id]
        center.add(UNNotificationRequest(identifier: "codexfold-incident-" + incident.id, content: content, trigger: nil))
    }

    func remove(_ incidentID: String) {
        active.remove(incidentID)
        let id = "codexfold-incident-" + incidentID
        center.removePendingNotificationRequests(withIdentifiers: [id])
        center.removeDeliveredNotifications(withIdentifiers: [id])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification, withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .list])
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse, withCompletionHandler completionHandler: @escaping () -> Void) {
        if response.actionIdentifier == UNNotificationDefaultActionIdentifier,
           let id = response.notification.request.content.userInfo["codexfoldIncidentID"] as? String {
            Task { @MainActor [weak self] in self?.onOpen?(id) }
        }
        completionHandler()
    }
}

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

    var incidentID: String? {
        guard let data = try? DurableAppGroupFile.read(url),
              let acknowledgement = try? JSONDecoder().decode(IncidentAcknowledgement.self, from: data) else {
            return nil
        }
        return acknowledgement.incidentID
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
    private let notifications: IncidentNotificationSending?
    private var lastNotified: IncidentAcknowledgement?
    private var latestIncident: FrontendIncident?

    init(
        appGroupURL: URL,
        diagnosticPayload: @escaping (FrontendIncident) -> Data?,
        presentWindow: ((NSWindowController) -> Void)? = nil,
        notifications: IncidentNotificationSending? = nil
    ) {
        acknowledgementStore = IncidentAcknowledgementStore(appGroupURL: appGroupURL)
        self.diagnosticPayload = diagnosticPayload
        self.presentWindow = presentWindow
        self.notifications = notifications ?? (presentWindow == nil ? SystemIncidentNotificationSender() : nil)
        super.init()
        self.notifications?.onOpen = { [weak self] id in
            guard let self, let incident = self.latestIncident, incident.id == id else { return }
            self.present(self.incidentForPresentation(incident), isNew: true, allowAcknowledged: true)
        }
    }

    func handle(_ update: IncidentUpdate) {
        switch update {
        case .active(let incident, let isNew):
            latestIncident = incident
            if let notifications {
                guard isNew, lastNotified?.matches(incident) != true, !acknowledgementStore.contains(incident) else { return }
                lastNotified = IncidentAcknowledgement(incident: incident)
                try? acknowledgementStore.acknowledge(incident)
                notifications.post(incident)
            } else {
                present(incidentForPresentation(incident), isNew: isNew)
            }
        case .recovered(let incident):
            notifications?.remove(incident.id)
            if let previous = lastNotified, previous.incidentID != incident.id { notifications?.remove(previous.incidentID) }
            lastNotified = nil
            latestIncident = nil
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
        if let id = acknowledgementStore.incidentID { notifications?.remove(id) }
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

    private func present(_ incident: FrontendIncident, isNew: Bool, allowAcknowledged: Bool = false) {
        if let incidentController {
            presentedIncident = incident
            updateWindow(incidentController, incident: incident)
            if isNew {
                show(incidentController)
            }
            return
        }
        guard isNew, allowAcknowledged || !acknowledgementStore.contains(incident) else { return }
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
    private let presenter: IncidentWindowPresenter?
    private let isNotificationHost: Bool
    private var timer: Timer?

    init(appGroupURL: URL, statusStore: StatusStore, isNotificationHost: Bool = true) {
        self.statusStore = statusStore
        lease = IncidentPresentationLease(appGroupURL: appGroupURL)
        self.isNotificationHost = isNotificationHost
        presenter = isNotificationHost ? IncidentWindowPresenter(
            appGroupURL: appGroupURL,
            diagnosticPayload: { [weak statusStore] incident in
                statusStore?.diagnosticPayload(incident: incident)
            }
        ) : nil
    }

    func start() {
        statusStore.onIncident = { [weak self] update in
            guard let self else { return }
            if !self.isNotificationHost {
                guard case .active(_, let isNew) = update, isNew,
                      let app = NSWorkspace.shared.urlForApplication(withBundleIdentifier: "vip.jstar.codexfold.fskitprofileprobe") else { return }
                let configuration = NSWorkspace.OpenConfiguration()
                configuration.activates = false
                NSWorkspace.shared.openApplication(at: app, configuration: configuration) { _, _ in }
                return
            }
            guard self.lease.isHeld else { return }
            self.presenter?.handle(update)
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
        guard isNotificationHost else { return }
        if statusStore.hasTrustedHealthyBaseline {
            presenter?.clearAcknowledgementAfterHealthyBaseline()
        }
        let alreadyHeld = lease.isHeld
        guard lease.acquire(), !alreadyHeld else { return }
        if let incident = statusStore.currentIncident {
            presenter?.handle(.active(incident, isNew: true))
        }
    }
}
