import Foundation
import XCTest
@testable import HeavyLockMonitor

final class RecentRunsTests: XCTestCase {
    private func record(
        _ pid: Int32, start: String = "100:1", state: StatusRecord.State = .running,
        stateChanged: String = "1970-01-01T00:00:00.000Z"
    ) -> StatusRecord {
        StatusRecord(
            version: 1, worklog: 123, attribution: "declared", command: ["swift", "test"],
            cwd: "/tmp", weight: 1, capacity: 2, state: state, reason: nil,
            pid: pid, processStart: start, created: stateChanged, stateChanged: stateChanged,
            heartbeat: stateChanged
        )
    }

    private func snapshot(_ records: [StatusRecord], readError: Bool = false) -> StatusSnapshot {
        StatusSnapshot(
            records: records,
            identity: { pid in records.first { $0.pid == pid }?.processStart },
            readError: readError
        )
    }

    func testMissingHolderIsRecordedOnceWithAdmissionAndObservedDuration() throws {
        let history = RecentRuns()
        let holder = record(1)
        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 1))
        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 5))
        XCTAssertTrue(history.entries.isEmpty)

        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 760))
        let entry = try XCTUnwrap(history.entries.first)
        XCTAssertEqual(history.entries.count, 1)
        XCTAssertEqual(entry.record.pid, 1)
        XCTAssertEqual(entry.admittedAt, Date(timeIntervalSince1970: 0))
        XCTAssertEqual(try XCTUnwrap(entry.duration), 760, accuracy: 0.001)
        XCTAssertEqual(entry.observedEnd, Date(timeIntervalSince1970: 760))

        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 761))
        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 762))
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 763))
        XCTAssertEqual(history.entries.count, 1)
    }

    func testWaiterDisappearanceDoesNotEnterHistory() {
        let history = RecentRuns()
        history.observe(snapshot([record(1, state: .waiting)]), at: Date(timeIntervalSince1970: 1))
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 2))
        XCTAssertTrue(history.entries.isEmpty)
    }

    func testUnreadableSnapshotsDeferRatherThanInventEndings() {
        let history = RecentRuns()
        let holder = record(1)
        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 1))
        history.observe(snapshot([], readError: true), at: Date(timeIntervalSince1970: 2))
        history.observe(
            StatusSnapshot(records: [holder], identity: { _ in nil }),
            at: Date(timeIntervalSince1970: 3)
        )
        XCTAssertTrue(history.entries.isEmpty)

        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 4))
        XCTAssertTrue(history.entries.isEmpty)
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 5))
        XCTAssertEqual(history.entries.map(\.record.pid), [1])
    }

    func testKnownHolderInOtherwiseUnknownSnapshotIsRemembered() {
        let history = RecentRuns()
        history.observe(snapshot([record(1)], readError: true), at: Date(timeIntervalSince1970: 1))
        XCTAssertTrue(history.entries.isEmpty)
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 2))
        XCTAssertEqual(history.entries.map(\.record.pid), [1])
    }

    func testConfirmedStaleAndReusedPidAreDifferentRuns() {
        let history = RecentRuns()
        let original = record(1, start: "100:1")
        let replacement = record(1, start: "200:1")
        history.observe(snapshot([original]), at: Date(timeIntervalSince1970: 1))

        let reused = StatusSnapshot(records: [original, replacement], identity: { _ in "200:1" })
        history.observe(reused, at: Date(timeIntervalSince1970: 2))
        XCTAssertEqual(history.entries.map(\.record.processStart), ["100:1"])
        history.observe(reused, at: Date(timeIntervalSince1970: 3))
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 4))
        XCTAssertEqual(history.entries.map(\.record.processStart), ["200:1", "100:1"])
    }

    func testConfirmedGoneProcessIsRecordedWithoutWaitingForStaleFileRemoval() {
        let history = RecentRuns()
        let holder = record(1)
        history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: 1))
        let stale = StatusSnapshot(records: [holder], identity: { _ in nil }, isGone: { _ in true })
        history.observe(stale, at: Date(timeIntervalSince1970: 2))
        XCTAssertEqual(history.entries.map(\.record.pid), [1])
    }

    func testRunsObservedEndingTogetherHaveDeterministicOrder() {
        let history = RecentRuns()
        history.observe(snapshot((1...8).reversed().map { record(Int32($0)) }), at: Date(timeIntervalSince1970: 1))
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 2))
        XCTAssertEqual(history.entries.map(\.record.pid), [1, 2, 3, 4, 5, 6, 7, 8])
        XCTAssertTrue(history.entries.allSatisfy { $0.observedEnd == Date(timeIntervalSince1970: 2) })
    }

    func testHistoryKeepsOnlyThe100MostRecentlyObservedEndings() {
        let history = RecentRuns()
        for index in 0...100 {
            let holder = record(Int32(index + 1000))
            history.observe(snapshot([holder]), at: Date(timeIntervalSince1970: Double(index * 2)))
            history.observe(snapshot([]), at: Date(timeIntervalSince1970: Double(index * 2 + 1)))
        }
        XCTAssertEqual(history.entries.count, 100)
        XCTAssertEqual(history.entries.first?.record.pid, 1100)
        XCTAssertEqual(history.entries.last?.record.pid, 1001)
    }

    func testInvalidAdmissionTimeDoesNotInventADuration() throws {
        let history = RecentRuns()
        history.observe(snapshot([record(1, stateChanged: "not a timestamp")]), at: Date(timeIntervalSince1970: 1))
        history.observe(snapshot([]), at: Date(timeIntervalSince1970: 2))
        let entry = try XCTUnwrap(history.entries.first)
        XCTAssertNil(entry.admittedAt)
        XCTAssertNil(entry.duration)
    }
}
