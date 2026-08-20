import Darwin
import Foundation
import XCTest

final class DurableAppGroupFileTests: XCTestCase {
    func testPublicationOrderAndRoundTripAreDurable() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let destination = root.appendingPathComponent("status.json")
        let payload = Data("{\"healthy\":true}\n".utf8)
        var steps: [DurableFilePublicationStep] = []

        try DurableAppGroupFile.write(payload, to: destination) { steps.append($0) }

        XCTAssertEqual(
            steps,
            [.createTemporary, .write, .syncFile, .rename, .verifyDestination, .syncDirectory]
        )
        XCTAssertEqual(try DurableAppGroupFile.read(destination), payload)
    }

    func testFileSyncFailureDoesNotPublishDestination() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let destination = root.appendingPathComponent("status.json")

        XCTAssertThrowsError(
            try DurableAppGroupFile.write(
                Data("pending".utf8),
                to: destination,
                failBefore: { $0 == .syncFile ? .EIO : nil }
            )
        )
        XCTAssertFalse(FileManager.default.fileExists(atPath: destination.path))
        XCTAssertFalse(try FileManager.default.contentsOfDirectory(atPath: root.path).contains { $0.hasSuffix(".tmp") })
    }

    func testDirectorySyncFailureIsReportedAfterAtomicPublication() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let destination = root.appendingPathComponent("ack.json")
        let payload = Data("ack".utf8)

        XCTAssertThrowsError(
            try DurableAppGroupFile.write(
                payload,
                to: destination,
                failBefore: { $0 == .syncDirectory ? .EIO : nil }
            )
        )
        XCTAssertEqual(try DurableAppGroupFile.read(destination), payload)
    }

    func testSymlinkAndFIFOAreRejectedWithoutFollowingOrBlocking() throws {
        let root = try temporaryDirectory()
        defer { try? FileManager.default.removeItem(at: root) }
        let external = root.appendingPathComponent("external")
        try Data("preserve".utf8).write(to: external)
        let symlink = root.appendingPathComponent("status.json")
        try FileManager.default.createSymbolicLink(at: symlink, withDestinationURL: external)

        XCTAssertThrowsError(try DurableAppGroupFile.write(Data("replace".utf8), to: symlink))
        XCTAssertEqual(try Data(contentsOf: external), Data("preserve".utf8))

        try FileManager.default.removeItem(at: symlink)
        XCTAssertEqual(Darwin.mkfifo(symlink.path, S_IRUSR | S_IWUSR), 0)
        let started = Date()
        XCTAssertThrowsError(try DurableAppGroupFile.read(symlink))
        XCTAssertLessThan(Date().timeIntervalSince(started), 0.25)
    }

    private func temporaryDirectory() throws -> URL {
        let root = FileManager.default.temporaryDirectory
            .appendingPathComponent("codexfold-durable-file-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        return root
    }
}
