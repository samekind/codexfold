import AppKit
import Foundation

@main
struct CodexFoldIncidentMonitorMain {
    private static let appGroupIdentifier = "group.vip.jstar.codexfold"

    @MainActor
    static func main() {
        guard let appGroupURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: appGroupIdentifier
        ), let runtimeRootURL = CodexFoldRuntimeLocation.rootURL(appGroupURL: appGroupURL) else {
            fputs("CodexFoldIncidentMonitor: app group is unavailable\n", stderr)
            exit(1)
        }
        let application = NSApplication.shared
        let delegate = CodexFoldIncidentMonitorDelegate(appGroupURL: runtimeRootURL)
        application.setActivationPolicy(.accessory)
        application.delegate = delegate
        withExtendedLifetime(delegate) {
            application.run()
        }
    }
}

@MainActor
private final class CodexFoldIncidentMonitorDelegate: NSObject, NSApplicationDelegate {
    private let appGroupURL: URL
    private var statusStore: StatusStore?
    private var coordinator: IncidentPresentationCoordinator?

    init(appGroupURL: URL) {
        self.appGroupURL = appGroupURL
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let store = StatusStore(
            appGroupURL: appGroupURL,
            monitorDaemonStatus: true,
            recordsHistory: false
        )
        let coordinator = IncidentPresentationCoordinator(appGroupURL: appGroupURL, statusStore: store)
        statusStore = store
        self.coordinator = coordinator
        coordinator.start()
        store.start()
    }

    func applicationWillTerminate(_ notification: Notification) {
        coordinator?.stop()
        statusStore?.stop()
    }
}
