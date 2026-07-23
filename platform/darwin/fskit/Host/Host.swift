import AppKit
import Darwin
import Dispatch
import Foundation

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
        NSApplication.shared.setActivationPolicy(.accessory)
        NSApplication.shared.terminate(nil)
    }

    private static func runCommand(_ arguments: [String]) throws -> Int32 {
        guard let root = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: appGroupIdentifier
        ) else {
            throw POSIXError(.ENOENT)
        }
        switch arguments.first {
        case "--app-group-path":
            print(root.path)
            return 0
        case "--app-group-write-probe":
            let probe = root.appendingPathComponent("host-write-probe", isDirectory: false)
            try Data("ok\n".utf8).write(to: probe, options: .atomic)
            try FileManager.default.removeItem(at: probe)
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
