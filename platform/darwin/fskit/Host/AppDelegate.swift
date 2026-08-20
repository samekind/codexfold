import AppKit
import SwiftUI

@MainActor
final class CodexFoldAppDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private var statusItem: NSStatusItem?
    private var popover: NSPopover?
    private var dashboardController: NSWindowController?
    private var statusStore: StatusStore?
    private var incidentCoordinator: IncidentPresentationCoordinator?

    func applicationDidFinishLaunching(_ notification: Notification) {
        let appGroupURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: "group.vip.jstar.codexfold"
        )
        let runtimeRootURL = CodexFoldRuntimeLocation.rootURL(appGroupURL: appGroupURL)
        let residency = ResidencyConfigurator.live().inspect()
        let store = StatusStore(
            appGroupURL: runtimeRootURL,
            monitorDaemonStatus: true,
            alertsBeforeRuntimeObserved: residency.incidentMonitor.isEnabled
                || residency.launchAtLogin.isEnabled
        )
        statusStore = store
        store.onChange = { [weak self] in self?.updateStatusItem() }
        if let runtimeRootURL {
            let coordinator = IncidentPresentationCoordinator(appGroupURL: runtimeRootURL, statusStore: store)
            incidentCoordinator = coordinator
            coordinator.start()
        }

        let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        self.statusItem = statusItem
        if let button = statusItem.button {
            button.image = statusSymbol(for: .unknown)
            button.imagePosition = .imageOnly
            button.toolTip = L10n.text(.productName)
            button.target = self
            button.action = #selector(togglePopover(_:))
            button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        }

        let popover = NSPopover()
        popover.behavior = .transient
        popover.animates = true
        popover.contentSize = NSSize(width: 360, height: 500)
        popover.contentViewController = NSHostingController(
            rootView: StatusPopoverView(
                store: store,
                openDashboard: { [weak self] in self?.openDashboard() },
                exportDiagnostics: { [weak self] in self?.exportDiagnostics() },
                copyDiagnostics: { [weak self] in self?.copyDiagnostics() },
                revealStatusFiles: { [weak self] in self?.revealStatusFiles() },
                quit: { NSApplication.shared.terminate(nil) }
            )
        )
        self.popover = popover
        store.start()
        store.reportHostNotice(residency.userNotice)

#if DEBUG
        if ProcessInfo.processInfo.environment["CODEXFOLD_UI_SHOW_POPOVER_ON_LAUNCH"] == "1" {
            DispatchQueue.main.asyncAfter(deadline: .now() + 0.6) { [weak self] in
                guard let self, let button = self.statusItem?.button else { return }
                self.statusStore?.refresh()
                self.updateStatusItem()
                self.popover?.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
                NSApp.activate(ignoringOtherApps: true)
            }
        }
#endif
    }

    func applicationWillTerminate(_ notification: Notification) {
        incidentCoordinator?.stop()
        statusStore?.stop()
    }

    func windowWillClose(_ notification: Notification) {
        if notification.object as AnyObject? === dashboardController?.window {
            dashboardController = nil
        }
    }

    @objc private func togglePopover(_ sender: NSStatusBarButton) {
        guard let event = NSApp.currentEvent else { return }
        if event.type == .rightMouseUp {
            showContextMenu(from: sender)
            return
        }
        guard let popover else { return }
        if popover.isShown {
            popover.performClose(sender)
        } else {
            statusStore?.refresh()
            popover.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY)
        }
    }

    @objc private func openDashboardAction() { openDashboard() }
    @objc private func checkStatusAction() { statusStore?.refresh() }
    @objc private func exportDiagnosticsAction() { exportDiagnostics() }
    @objc private func copyDiagnosticsAction() { copyDiagnostics() }
    @objc private func revealStatusFilesAction() { revealStatusFiles() }
    @objc private func quitAction() { NSApplication.shared.terminate(nil) }

    private func showContextMenu(from button: NSStatusBarButton) {
        let menu = NSMenu()
        menu.addItem(withTitle: L10n.text(.openDashboard), action: #selector(openDashboardAction), keyEquivalent: "")
        menu.addItem(.separator())
        menu.addItem(withTitle: L10n.text(.checkStatus), action: #selector(checkStatusAction), keyEquivalent: "")
        menu.addItem(withTitle: L10n.text(.exportDiagnostics), action: #selector(exportDiagnosticsAction), keyEquivalent: "")
        menu.addItem(withTitle: L10n.text(.copyDiagnostics), action: #selector(copyDiagnosticsAction), keyEquivalent: "")
        menu.addItem(withTitle: L10n.text(.revealStatusFiles), action: #selector(revealStatusFilesAction), keyEquivalent: "")
        menu.addItem(.separator())
        menu.addItem(withTitle: L10n.text(.quit), action: #selector(quitAction), keyEquivalent: "q")
        menu.items.forEach { $0.target = self }
        statusItem?.menu = menu
        button.performClick(nil)
        statusItem?.menu = nil
    }

    private func updateStatusItem() {
        guard let store = statusStore, let button = statusItem?.button else { return }
        let popoverHeight: CGFloat = store.isRuntimeUnconnected ? 300 : 500
        if popover?.contentSize.height != popoverHeight {
            popover?.contentSize = NSSize(width: 360, height: popoverHeight)
        }
        button.image = statusSymbol(for: store.overallHealth)
        button.title = store.menuBarTitle
        button.imagePosition = store.menuBarTitle.isEmpty ? .imageOnly : .imageLeading
        button.toolTip = store.storageMetrics.map {
            "\(store.summary) · \(L10n.text(.spaceSaved)) \(ByteCountFormatter.string(fromByteCount: $0.savedBytes, countStyle: .file))"
        } ?? store.summary
        button.contentTintColor = statusColor(for: store.overallHealth)
        button.setAccessibilityLabel(L10n.text(.productName))
        button.setAccessibilityValue(button.toolTip ?? store.summary)
    }

    private func openDashboard() {
        popover?.performClose(nil)
        if let dashboardController {
            dashboardController.showWindow(nil)
            dashboardController.window?.makeKeyAndOrderFront(nil)
            NSApp.activate(ignoringOtherApps: true)
            return
        }
        guard let store = statusStore else { return }
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 900, height: 760),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = L10n.text(.productName)
        window.minSize = NSSize(width: 780, height: 650)
        window.center()
        window.isReleasedWhenClosed = false
        window.delegate = self
        window.contentViewController = NSHostingController(
            rootView: DashboardView(
                store: store,
                exportDiagnostics: { [weak self] in self?.exportDiagnostics() },
                copyDiagnostics: { [weak self] in self?.copyDiagnostics() },
                revealStatusFiles: { [weak self] in self?.revealStatusFiles() }
            )
        )
        let controller = NSWindowController(window: window)
        dashboardController = controller
        controller.showWindow(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func exportDiagnostics(incident: FrontendIncident? = nil) {
        guard let data = statusStore?.diagnosticPayload(incident: incident) else { return }
        let panel = NSSavePanel()
        panel.nameFieldStringValue = "codexfold-diagnostics-\(Self.filenameTimestamp.string(from: Date())).json"
        panel.allowedContentTypes = [.json]
        panel.canCreateDirectories = true
        panel.begin { response in
            guard response == .OK, let url = panel.url else { return }
            do {
                try data.write(to: url, options: [.atomic])
            } catch {
                let alert = NSAlert(error: error)
                alert.messageText = L10n.text(.diagnosticsSaveFailed)
                alert.runModal()
            }
        }
    }

    private func copyDiagnostics(incident: FrontendIncident? = nil) {
        guard let data = statusStore?.diagnosticPayload(incident: incident),
              let value = String(data: data, encoding: .utf8) else { return }
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(value, forType: .string)
    }

    private func revealStatusFiles() {
        guard let directory = statusStore?.statusDirectoryURL else { return }
        NSWorkspace.shared.activateFileViewerSelecting([directory])
    }

    private func statusSymbol(for health: ComponentHealth) -> NSImage? {
        let name: String
        switch health {
        case .healthy: name = "checkmark.circle.fill"
        case .recovering: name = "arrow.triangle.2.circlepath.circle.fill"
        case .failed: name = "exclamationmark.triangle.fill"
        case .unknown: name = "questionmark.circle"
        }
        let image = NSImage(systemSymbolName: name, accessibilityDescription: L10n.text(.productName))
        image?.isTemplate = false
        return image
    }

    private func statusColor(for health: ComponentHealth) -> NSColor {
        switch health {
        case .healthy: return .systemGreen
        case .recovering: return .systemOrange
        case .failed: return .systemRed
        case .unknown: return .secondaryLabelColor
        }
    }

    private static let filenameTimestamp: DateFormatter = {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        return formatter
    }()
}
