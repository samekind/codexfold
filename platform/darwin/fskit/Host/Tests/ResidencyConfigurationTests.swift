import Foundation
import XCTest

final class ResidencyConfigurationTests: XCTestCase {
    func testInspectionNeverRegistersServices() {
        let monitor = FakeResidencyService(state: .notRegistered, stateAfterRegister: .enabled)
        let login = FakeResidencyService(state: .notRegistered, stateAfterRegister: .enabled)
        let legacy = FakeResidencyService(state: .enabled, stateAfterUnregister: .notRegistered)

        let outcome = ResidencyConfigurator(
            incidentMonitor: monitor,
            launchAtLogin: login,
            legacyLaunchAtLogin: legacy
        ).inspect()

        XCTAssertFalse(outcome.ready)
        XCTAssertFalse(outcome.requiresApproval)
        XCTAssertEqual(outcome.incidentMonitor.state, .notRegistered)
        XCTAssertEqual(outcome.launchAtLogin.state, .notRegistered)
        XCTAssertEqual(monitor.registerCalls, 0)
        XCTAssertEqual(login.registerCalls, 0)
        XCTAssertEqual(legacy.unregisterCalls, 0)
        XCTAssertEqual(outcome.userNotice, L10n.text(.incidentMonitorNotConfigured))
    }

    func testNotRegisteredServicesAreRegisteredAndReportedReady() {
        let monitor = FakeResidencyService(state: .notRegistered, stateAfterRegister: .enabled)
        let login = FakeResidencyService(state: .notRegistered, stateAfterRegister: .enabled)
        let legacy = FakeResidencyService(state: .enabled, stateAfterUnregister: .notRegistered)

        let outcome = ResidencyConfigurator(
            incidentMonitor: monitor,
            launchAtLogin: login,
            legacyLaunchAtLogin: legacy
        ).configure()

        XCTAssertTrue(outcome.ready)
        XCTAssertFalse(outcome.requiresApproval)
        XCTAssertEqual(monitor.registerCalls, 1)
        XCTAssertEqual(login.registerCalls, 1)
        XCTAssertEqual(legacy.unregisterCalls, 1)
        XCTAssertNil(outcome.userNotice)
    }

    func testLegacyLoginItemRetirementFailureBlocksDuplicateMenuBarRegistration() {
        let monitor = FakeResidencyService(state: .enabled)
        let residentAgent = FakeResidencyService(state: .notRegistered, stateAfterRegister: .enabled)
        let legacy = FakeResidencyService(
            state: .enabled,
            unregisterError: TestRegistrationError.failed
        )

        let outcome = ResidencyConfigurator(
            incidentMonitor: monitor,
            launchAtLogin: residentAgent,
            legacyLaunchAtLogin: legacy
        ).configure()

        XCTAssertFalse(outcome.ready)
        XCTAssertEqual(outcome.launchAtLogin.state, .unavailable)
        XCTAssertNotNil(outcome.launchAtLogin.detail)
        XCTAssertEqual(legacy.unregisterCalls, 1)
        XCTAssertEqual(residentAgent.registerCalls, 0)
    }

    func testApprovalRequirementIsExplicitAndDoesNotRetryRegistration() {
        let monitor = FakeResidencyService(state: .requiresApproval)
        let login = FakeResidencyService(state: .enabled)

        let outcome = ResidencyConfigurator(
            incidentMonitor: monitor,
            launchAtLogin: login
        ).configure()

        XCTAssertFalse(outcome.ready)
        XCTAssertTrue(outcome.requiresApproval)
        XCTAssertEqual(outcome.incidentMonitor.state, .requiresApproval)
        XCTAssertEqual(monitor.registerCalls, 0)
        XCTAssertEqual(login.registerCalls, 0)
        XCTAssertEqual(outcome.userNotice, L10n.text(.incidentMonitorRequiresApproval))
    }

    func testRegistrationFailureIsFailClosedAndCarriesTechnicalOutcome() {
        let monitor = FakeResidencyService(
            state: .notRegistered,
            registerError: TestRegistrationError.failed
        )
        let login = FakeResidencyService(state: .enabled)

        let outcome = ResidencyConfigurator(
            incidentMonitor: monitor,
            launchAtLogin: login
        ).configure()

        XCTAssertFalse(outcome.ready)
        XCTAssertFalse(outcome.requiresApproval)
        XCTAssertEqual(outcome.incidentMonitor.state, .unavailable)
        XCTAssertNotNil(outcome.incidentMonitor.detail)
        XCTAssertEqual(monitor.registerCalls, 1)
        XCTAssertEqual(outcome.userNotice, L10n.text(.incidentMonitorUnavailable))
    }

    func testEncodedOutcomeExposesReadinessInsteadOfImplyingSuccess() throws {
        let outcome = ResidencyConfigurationOutcome(
            incidentMonitor: ResidencyServiceOutcome(state: .requiresApproval, detail: nil),
            launchAtLogin: ResidencyServiceOutcome(state: .enabled, detail: nil)
        )
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        let data = try encoder.encode(outcome)
        let object = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: data) as? [String: Any]
        )

        XCTAssertEqual(object["schema_version"] as? Int, 1)
        XCTAssertEqual(object["ready"] as? Bool, false)
        XCTAssertEqual(object["requires_approval"] as? Bool, true)
        XCTAssertNotNil(object["incident_monitor"])
        XCTAssertNotNil(object["launch_at_login"])
    }

    func testMenuBarLaunchAgentRestartsCrashesWithoutOverridingExplicitQuit() throws {
        let menuBar = try launchAgent(named: "CodexFoldMenuBar.plist")
        XCTAssertEqual(menuBar["Label"] as? String, "vip.jstar.codexfold.fskitprofileprobe.menu-bar")
        XCTAssertEqual(menuBar["BundleProgram"] as? String, "Contents/MacOS/CodexFoldFSKit")
        XCTAssertEqual(menuBar["ProgramArguments"] as? [String], ["CodexFoldFSKit"])
        XCTAssertEqual(menuBar["RunAtLoad"] as? Bool, true)
        XCTAssertEqual(menuBar["ProcessType"] as? String, "Interactive")
        XCTAssertEqual(menuBar["LimitLoadToSessionType"] as? String, "Aqua")
        XCTAssertEqual(menuBar["ThrottleInterval"] as? Int, 2)
        let keepAlive = try XCTUnwrap(menuBar["KeepAlive"] as? [String: Any])
        XCTAssertEqual(keepAlive["SuccessfulExit"] as? Bool, false)
    }

    func testMenuBarAndIncidentMonitorUseIndependentLaunchAgents() throws {
        let menuBar = try launchAgent(named: "CodexFoldMenuBar.plist")
        let incident = try launchAgent(named: "CodexFoldIncidentMonitor.plist")
        XCTAssertNotEqual(menuBar["Label"] as? String, incident["Label"] as? String)
        XCTAssertNotEqual(menuBar["BundleProgram"] as? String, incident["BundleProgram"] as? String)
    }

    private func launchAgent(named name: String) throws -> [String: Any] {
        let url = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent(name, isDirectory: false)
        let data = try Data(contentsOf: url)
        let propertyList = try PropertyListSerialization.propertyList(
            from: data,
            options: [],
            format: nil
        )
        return try XCTUnwrap(propertyList as? [String: Any])
    }
}

private enum TestRegistrationError: Error {
    case failed
}

private final class FakeResidencyService: ResidencyServiceControlling {
    private(set) var state: ResidencyServiceState
    private let stateAfterRegister: ResidencyServiceState
    private let stateAfterUnregister: ResidencyServiceState
    private let registerError: Error?
    private let unregisterError: Error?
    private(set) var registerCalls = 0
    private(set) var unregisterCalls = 0

    init(
        state: ResidencyServiceState,
        stateAfterRegister: ResidencyServiceState? = nil,
        stateAfterUnregister: ResidencyServiceState = .notRegistered,
        registerError: Error? = nil,
        unregisterError: Error? = nil
    ) {
        self.state = state
        self.stateAfterRegister = stateAfterRegister ?? state
        self.stateAfterUnregister = stateAfterUnregister
        self.registerError = registerError
        self.unregisterError = unregisterError
    }

    func register() throws {
        registerCalls += 1
        if let registerError {
            throw registerError
        }
        state = stateAfterRegister
    }

    func unregister() throws {
        unregisterCalls += 1
        if let unregisterError {
            throw unregisterError
        }
        state = stateAfterUnregister
    }
}
