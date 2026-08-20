import Foundation
import ServiceManagement

enum ResidencyServiceState: String, Codable, Equatable {
    case enabled
    case notRegistered = "not_registered"
    case requiresApproval = "requires_approval"
    case notFound = "not_found"
    case unavailable
}

struct ResidencyServiceOutcome: Codable, Equatable {
    let state: ResidencyServiceState
    let detail: String?

    var isEnabled: Bool { state == .enabled }
    var requiresApproval: Bool { state == .requiresApproval }
}

struct ResidencyConfigurationOutcome: Codable, Equatable {
    let schemaVersion: Int
    let incidentMonitor: ResidencyServiceOutcome
    let launchAtLogin: ResidencyServiceOutcome
    let ready: Bool
    let requiresApproval: Bool

    init(
        incidentMonitor: ResidencyServiceOutcome,
        launchAtLogin: ResidencyServiceOutcome
    ) {
        schemaVersion = 1
        self.incidentMonitor = incidentMonitor
        self.launchAtLogin = launchAtLogin
        ready = incidentMonitor.isEnabled && launchAtLogin.isEnabled
        requiresApproval = incidentMonitor.requiresApproval || launchAtLogin.requiresApproval
    }

    var userNotice: String? {
        if incidentMonitor.state == .notRegistered {
            return L10n.text(.incidentMonitorNotConfigured)
        }
        if incidentMonitor.requiresApproval {
            return L10n.text(.incidentMonitorRequiresApproval)
        }
        if !incidentMonitor.isEnabled {
            return L10n.text(.incidentMonitorUnavailable)
        }
        if launchAtLogin.state == .notRegistered {
            return L10n.text(.loginItemNotConfigured)
        }
        if launchAtLogin.requiresApproval {
            return L10n.text(.loginItemRequiresApproval)
        }
        if !launchAtLogin.isEnabled {
            return L10n.text(.loginItemUnavailable)
        }
        return nil
    }
}

protocol ResidencyServiceControlling {
    var state: ResidencyServiceState { get }
    func register() throws
    func unregister() throws
}

private final class SystemResidencyServiceController: ResidencyServiceControlling {
    private let service: SMAppService

    init(service: SMAppService) {
        self.service = service
    }

    var state: ResidencyServiceState {
        switch service.status {
        case .enabled: return .enabled
        case .notRegistered: return .notRegistered
        case .requiresApproval: return .requiresApproval
        case .notFound: return .notFound
        @unknown default: return .unavailable
        }
    }

    func register() throws {
        try service.register()
    }

    func unregister() throws {
        try service.unregister()
    }
}

struct ResidencyConfigurator {
    let incidentMonitor: ResidencyServiceControlling
    let launchAtLogin: ResidencyServiceControlling
    let legacyLaunchAtLogin: ResidencyServiceControlling?

    init(
        incidentMonitor: ResidencyServiceControlling,
        launchAtLogin: ResidencyServiceControlling,
        legacyLaunchAtLogin: ResidencyServiceControlling? = nil
    ) {
        self.incidentMonitor = incidentMonitor
        self.launchAtLogin = launchAtLogin
        self.legacyLaunchAtLogin = legacyLaunchAtLogin
    }

    static func live() -> ResidencyConfigurator {
        ResidencyConfigurator(
            incidentMonitor: SystemResidencyServiceController(
                service: SMAppService.agent(plistName: "CodexFoldIncidentMonitor.plist")
            ),
            launchAtLogin: SystemResidencyServiceController(
                service: SMAppService.agent(plistName: "CodexFoldMenuBar.plist")
            ),
            legacyLaunchAtLogin: SystemResidencyServiceController(service: SMAppService.mainApp)
        )
    }

    func configure() -> ResidencyConfigurationOutcome {
        let launchAtLoginOutcome: ResidencyServiceOutcome
        do {
            try retireLegacyLaunchAtLoginIfNeeded()
            launchAtLoginOutcome = configure(launchAtLogin)
        } catch {
            launchAtLoginOutcome = ResidencyServiceOutcome(
                state: .unavailable,
                detail: "retire legacy login item: \(String(describing: error))"
            )
        }
        return ResidencyConfigurationOutcome(
            incidentMonitor: configure(incidentMonitor),
            launchAtLogin: launchAtLoginOutcome
        )
    }

    func inspect() -> ResidencyConfigurationOutcome {
        ResidencyConfigurationOutcome(
            incidentMonitor: inspect(incidentMonitor),
            launchAtLogin: inspect(launchAtLogin)
        )
    }

    private func inspect(_ service: ResidencyServiceControlling) -> ResidencyServiceOutcome {
        ResidencyServiceOutcome(state: service.state, detail: nil)
    }

    private func configure(_ service: ResidencyServiceControlling) -> ResidencyServiceOutcome {
        switch service.state {
        case .enabled:
            return ResidencyServiceOutcome(state: .enabled, detail: nil)
        case .requiresApproval:
            return ResidencyServiceOutcome(state: .requiresApproval, detail: nil)
        case .notFound:
            return ResidencyServiceOutcome(state: .notFound, detail: nil)
        case .unavailable:
            return ResidencyServiceOutcome(state: .unavailable, detail: nil)
        case .notRegistered:
            do {
                try service.register()
            } catch {
                return ResidencyServiceOutcome(
                    state: .unavailable,
                    detail: String(describing: error)
                )
            }
            switch service.state {
            case .enabled:
                return ResidencyServiceOutcome(state: .enabled, detail: nil)
            case .requiresApproval:
                return ResidencyServiceOutcome(state: .requiresApproval, detail: nil)
            case .notFound:
                return ResidencyServiceOutcome(state: .notFound, detail: nil)
            case .notRegistered, .unavailable:
                return ResidencyServiceOutcome(
                    state: .unavailable,
                    detail: "registration did not produce an enabled or approval-pending service"
                )
            }
        }
    }

    private func retireLegacyLaunchAtLoginIfNeeded() throws {
        guard let legacyLaunchAtLogin else { return }
        switch legacyLaunchAtLogin.state {
        case .enabled, .requiresApproval:
            try legacyLaunchAtLogin.unregister()
            guard legacyLaunchAtLogin.state != .enabled,
                  legacyLaunchAtLogin.state != .requiresApproval else {
                throw ResidencyConfigurationError.legacyLoginItemStillRegistered
            }
        case .notRegistered, .notFound, .unavailable:
            return
        }
    }
}

private enum ResidencyConfigurationError: Error {
    case legacyLoginItemStillRegistered
}
