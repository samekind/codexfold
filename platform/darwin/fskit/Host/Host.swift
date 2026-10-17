import AppKit
import Darwin
import Dispatch
import Foundation
import ServiceManagement
import UserNotifications

@main
struct CodexFoldFSKitHost {
    private static let appGroupIdentifier = "group.vip.jstar.codexfold"
    private static let inheritedEnvironmentKeys = [
        "HOME",
        "PATH",
        "TMPDIR",
        "USER",
        "LOGNAME",
        "LANG",
        "LC_ALL",
        "LC_CTYPE",
        "LC_NUMERIC",
        "LC_TIME",
        "LC_COLLATE",
        "LC_MONETARY",
        "LC_MESSAGES",
        "LC_PAPER",
        "LC_NAME",
        "LC_ADDRESS",
        "LC_TELEPHONE",
        "LC_MEASUREMENT",
        "LC_IDENTIFICATION",
    ]

    static func main() {
        if CommandLine.arguments.count > 1 {
            do {
                exit(try runCommand(Array(CommandLine.arguments.dropFirst())))
            } catch {
                fputs("CodexFoldFSKit: \(error)\n", stderr)
                exit(1)
            }
        }
        let application = NSApplication.shared
        let delegate = CodexFoldAppDelegate()
        application.setActivationPolicy(.accessory)
        application.delegate = delegate
        withExtendedLifetime(delegate) {
            application.run()
        }
    }

    private static func runCommand(_ arguments: [String]) throws -> Int32 {
        guard let root = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: appGroupIdentifier
        ) else {
            throw POSIXError(.ENOENT)
        }
        switch arguments.first {
        case "--app-group-path", "--print-app-group":
            print(root.path)
            return 0
        case "--app-group-write-probe":
            let probe = root.appendingPathComponent("host-write-probe", isDirectory: false)
            try Data("ok\n".utf8).write(to: probe, options: .atomic)
            try FileManager.default.removeItem(at: probe)
            return 0
        case "--configure-residency":
            let encoder = JSONEncoder()
            encoder.keyEncodingStrategy = .convertToSnakeCase
            encoder.outputFormatting = [.sortedKeys]
            let payload = try encoder.encode(ResidencyConfigurator.live().configure())
            guard let output = String(data: payload, encoding: .utf8) else {
                throw POSIXError(.EILSEQ)
            }
            print(output)
            return 0
        case "--refresh-incident-monitor":
            let service = SMAppService.agent(plistName: "CodexFoldIncidentMonitor.plist")
            try service.unregister()
            try service.register()
            print("incident-monitor registration refreshed")
            return 0
        case "--notification-test":
            let center = UNUserNotificationCenter.current()
            let finished = DispatchSemaphore(value: 0)
            center.requestAuthorization(options: [.alert, .provisional]) { granted, error in
                guard granted else {
                    print("notifications unavailable: \(error?.localizedDescription ?? "permission denied")")
                    finished.signal()
                    return
                }
                let content = UNMutableNotificationContent()
                content.title = "CodexFold"
                content.body = "通知通道测试：故障将通过系统通知提示，不会自动弹出窗口。"
                content.sound = nil
                center.add(UNNotificationRequest(identifier: "codexfold-notification-channel-test", content: content, trigger: nil)) { error in
                    print(error.map { "notification submission failed: \($0.localizedDescription)" } ?? "notification submitted")
                    finished.signal()
                }
            }
            guard finished.wait(timeout: .now() + 10) == .success else { throw POSIXError(.ETIMEDOUT) }
            return 0
        case "--notification-status":
            let center = UNUserNotificationCenter.current()
            let finished = DispatchSemaphore(value: 0)
            center.getNotificationSettings { settings in
                center.getDeliveredNotifications { notifications in
                    let report: [String: Any] = [
                        "authorization_status": settings.authorizationStatus.rawValue,
                        "notification_center_enabled": settings.notificationCenterSetting == .enabled,
                        "delivered_requests": notifications.map { $0.request.identifier }.filter { $0.hasPrefix("codexfold-") }.sorted(),
                    ]
                    if let data = try? JSONSerialization.data(withJSONObject: report, options: [.sortedKeys]),
                       let output = String(data: data, encoding: .utf8) { print(output) }
                    finished.signal()
                }
            }
            guard finished.wait(timeout: .now() + 10) == .success else { throw POSIXError(.ETIMEDOUT) }
            return 0
        case "--clear-notification-test":
            let center = UNUserNotificationCenter.current()
            center.removePendingNotificationRequests(withIdentifiers: ["codexfold-notification-channel-test"])
            center.removeDeliveredNotifications(withIdentifiers: ["codexfold-notification-channel-test"])
            return 0
        case "--run-helper":
            guard arguments.count >= 2 else {
                throw POSIXError(.EINVAL)
            }
            return try runHelper(executable: arguments[1], arguments: Array(arguments.dropFirst(2)))
        default:
            throw POSIXError(.EINVAL)
        }
    }

    private static func runHelper(executable: String, arguments: [String]) throws -> Int32 {
        guard executable.hasPrefix("/") else {
            throw POSIXError(.EINVAL)
        }
        let process = Process()
        process.executableURL = URL(fileURLWithPath: executable)
        process.arguments = arguments
        process.standardInput = FileHandle.standardInput
        process.standardOutput = FileHandle.standardOutput
        process.standardError = FileHandle.standardError

        var environment: [String: String] = [:]
        let parentEnvironment = ProcessInfo.processInfo.environment
        for key in inheritedEnvironmentKeys {
            if let value = parentEnvironment[key] {
                environment[key] = value
            }
        }
        environment["CODEXFOLD_LAUNCHER_PARENT_PID"] = String(Darwin.getpid())
        process.environment = environment

        var signalSources: [DispatchSourceSignal] = []
        for signalNumber in [SIGTERM, SIGINT, SIGHUP] {
            Darwin.signal(signalNumber, SIG_IGN)
            let source = DispatchSource.makeSignalSource(signal: signalNumber, queue: .global())
            source.setEventHandler {
                if process.isRunning {
                    _ = Darwin.kill(process.processIdentifier, signalNumber)
                }
            }
            source.resume()
            signalSources.append(source)
        }

        try process.run()
        process.waitUntilExit()
        signalSources.forEach { $0.cancel() }
        if process.terminationReason == .uncaughtSignal {
            return 128 + process.terminationStatus
        }
        return process.terminationStatus
    }
}
