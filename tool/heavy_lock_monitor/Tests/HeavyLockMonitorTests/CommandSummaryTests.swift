import Foundation
import XCTest
@testable import HeavyLockMonitor

final class CommandSummaryTests: XCTestCase {
    func testCoverageShellCommandUsesTheFirstTestCommand() {
        let command = [
            "bash", "-c",
            #"fvm flutter test --no-pub --coverage "--coverage-package=^(core|api|data)$" --concurrency=4 && (cd packages/api && fvm flutter test --coverage)"#,
        ]
        XCTAssertEqual(summarizeCommand(command), "fvm flutter test …")
    }

    func testShellSummarySkipsLeadingChangesOfDirectoryAndGrouping() {
        XCTAssertEqual(
            summarizeCommand(["bash", "-c", "cd packages/api && fvm flutter test --concurrency=2"]),
            "fvm flutter test …"
        )
        XCTAssertEqual(
            summarizeCommand(["/bin/sh", "-c", "(cd 'pkg/some dir' && (cd nested && swift test))"]),
            "swift test …"
        )
    }

    func testInterpreterScriptsUseOnlyTheirFileNames() {
        let cases: [([String], String)] = [
            (["python3", "tool/run_checks.py", "--suite", "abc"], "run_checks.py"),
            (["/usr/bin/python", "-u", "tool/run_checks.py"], "run_checks.py"),
            (["python3", "-uW", "ignore", "tool/run_checks.py"], "run_checks.py"),
            (["bash", "tool/heavy_lock_monitor/build.sh", "launch"], "build.sh"),
            (["dart", "bin/heavy_lock.dart", "--worklog", "123"], "heavy_lock.dart"),
            (["fvm", "dart", "bin/heavy_lock.dart", "--worklog", "123"], "heavy_lock.dart"),
            (["node", "tool/check.js", "--option"], "check.js"),
            (["sh", "-c", "python3 'tool/my task.py' --flag"], "my task.py"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testInterpreterOptionValuesAreNotMistakenForScriptNames() {
        let cases: [([String], String)] = [
            (["python3", "--check-hash-based-pycs", "always", "tool/run_checks.py"], "run_checks.py"),
            (["python3", "--check-hash-based-pycs", "always"], "python3 …"),
            (["node", "--import", "tsx", "tool/check.js"], "check.js"),
            (["node", "--import", "tsx"], "node …"),
            (["node", "-C", "development", "tool/check.js"], "check.js"),
            (["node", "--conditions", "development", "tool/check.js"], "check.js"),
            (["node", "--import=tsx", "tool/check.js"], "check.js"),
            (["node", "--inspect", "tool/check.js"], "check.js"),
            (["node", "-c", "tool/check.js"], "check.js"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testSubcommandsStayVisibleAndOmittedArgumentsAreMarked() {
        let cases: [([String], String)] = [
            (["swift", "test"], "swift test"),
            (["/usr/bin/swift", "test"], "swift test"),
            (["fvm", "flutter", "build", "apk", "--release"], "fvm flutter build apk …"),
            (["swift", "test", "Tests/LongTests.swift"], "swift test …"),
            (["echo", "hello world"], "echo …"),
            (["sh", "-c", "swift test && swift build"], "swift test …"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testDartSubcommandsAreNotMistakenForScripts() {
        let cases: [([String], String)] = [
            (["dart", "analyze"], "dart analyze"),
            (["fvm", "dart", "test", "--coverage"], "fvm dart test …"),
            (["dart", "compile", "exe", "tool/job.dart"], "dart compile exe …"),
            (["dart", "run", "build_runner", "build"], "dart run build_runner build"),
            (["dart", "run", "bin/heavy_lock.dart", "--worklog", "123"], "heavy_lock.dart"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testShellNewlinesSeparateCommandsButEscapedNewlinesContinueThem() {
        XCTAssertEqual(
            summarizeCommand(["bash", "-c", "swift test\nswift build"]),
            "swift test …"
        )
        XCTAssertEqual(
            summarizeCommand(["sh", "-c", "fvm \\\nflutter test --coverage"]),
            "fvm flutter test …"
        )
        XCTAssertEqual(
            summarizeCommand(["bash", "-c", "python3 \"tool/run_\\\nchecks.py\" --suite abc"]),
            "run_checks.py"
        )
        XCTAssertEqual(
            summarizeCommand(["sh", "-c", "\n(cd packages/api &&\nfvm flutter test --coverage)\n"]),
            "fvm flutter test …"
        )
        XCTAssertEqual(summarizeCommand(["sh", "-c", "swift test\n"]), "swift test")
    }

    func testShellRedirectionDoesNotInventAnArgument() {
        XCTAssertEqual(summarizeCommand(["sh", "-c", "swift test 2>&1 | tail"]), "swift test …")
        XCTAssertEqual(summarizeCommand(["sh", "-c", "echo 2 >output"]), "echo 2 …")
    }

    func testShellOptionsWithValuesDoNotBecomeScriptNames() {
        let cases = [
            ["bash", "-o", "pipefail", "tool/heavy_lock_monitor/build.sh", "launch"],
            ["bash", "-O", "extglob", "tool/heavy_lock_monitor/build.sh"],
            ["bash", "-euo", "pipefail", "tool/heavy_lock_monitor/build.sh"],
            ["bash", "+o", "pipefail", "tool/heavy_lock_monitor/build.sh"],
            ["bash", "-r", "tool/heavy_lock_monitor/build.sh"],
            ["bash", "-", "tool/heavy_lock_monitor/build.sh"],
            ["sh", "-o", "errexit", "tool/heavy_lock_monitor/build.sh"],
        ]
        for command in cases {
            XCTAssertEqual(summarizeCommand(command), "build.sh", "\(command)")
        }
    }

    func testShellCommandFlagsCanBeCombinedOrFollowOptionsWithValues() {
        let cases: [([String], String)] = [
            (["bash", "-lc", "cd pkg && python3 tool/run_checks.py --suite abc"], "run_checks.py"),
            (["bash", "+c", "python3 tool/run_checks.py --suite abc"], "run_checks.py"),
            (["sh", "-e", "-c", "fvm flutter test --coverage"], "fvm flutter test …"),
            (["bash", "-euo", "pipefail", "-c", "cd pkg && fvm flutter test --coverage"], "fvm flutter test …"),
            (["bash", "-co", "pipefail", "cd pkg && swift test"], "swift test …"),
            (["bash", "-c", "-e", "swift test"], "swift test"),
            (["bash", "-c", "-", "cd pkg && swift test"], "swift test …"),
            (["bash", "--noprofile", "-lc", "python3 tool/run_checks.py --suite abc"], "run_checks.py"),
            (["bash", "-login", "-c", "python3 tool/run_checks.py --suite abc"], "run_checks.py"),
            (["bash", "-noediting", "-c", "python3 tool/run_checks.py --suite abc"], "run_checks.py"),
            (["bash", "-rcfile", "/dev/null", "-c", "swift test"], "swift test"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testUnknownAndIncompleteCommandsHaveHonestFallbacks() {
        let cases: [([String], String)] = [
            (["some-tool", "--option", "value"], "some-tool …"),
            (["some-tool", "inspect", "--option"], "some-tool inspect …"),
            (["python3", "-m", "http.server"], "python3 …"),
            (["python3", "-c", "print('hello')"], "python3 …"),
            (["python3", "-uc", "print(open('a/b').read())"], "python3 …"),
            (["python3", "-um", "http.server"], "python3 …"),
            (["python3", "-", "not-a-script"], "python3 …"),
            (["python3", "--unknown-option", "not-a-script", "tool/run_checks.py"], "python3 …"),
            (["python3", "-Z", "not-a-script", "tool/run_checks.py"], "python3 …"),
            (["node", "-pe", "process.version"], "node …"),
            (["node", "-", "not-a-script"], "node …"),
            (["node", "--unknown-option", "not-a-script", "tool/check.js"], "node …"),
            (["node", "-z", "not-a-script", "tool/check.js"], "node …"),
            (["node", "-w", "not-a-script", "tool/check.js"], "node …"),
            (["node", "--eval=console.log(1)", "not-a-script"], "node …"),
            (["bash", "-c"], "bash …"),
            (["bash", "-s", "not-a-script"], "bash …"),
            (["bash", "+s", "not-a-script"], "bash …"),
            (["bash", "-c", "   "], "bash …"),
            ([], "Command unavailable"),
        ]
        for (command, expected) in cases {
            XCTAssertEqual(summarizeCommand(command), expected, "\(command)")
        }
    }

    func testNamesWithLineBreaksStayOnOneLine() {
        XCTAssertEqual(summarizeCommand(["/tmp/my\nrunner", "--flag"]), "my runner …")
        XCTAssertEqual(summarizeCommand(["python3", "tool/my\nscript.py"]), "my script.py")
    }

    func testFullCommandPreservesArgumentBoundariesWhenPasted() throws {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/bin/sh")
        process.arguments = ["-c", fullCommand(["/usr/bin/printf", "<%s>|<%s>|<%s>", "two words", "it's fine", ""])]
        let output = Pipe()
        process.standardOutput = output
        process.standardError = FileHandle.nullDevice
        try process.run()
        let data = output.fileHandleForReading.readDataToEndOfFile()
        process.waitUntilExit()
        XCTAssertEqual(process.terminationStatus, 0)
        XCTAssertEqual(String(data: data, encoding: .utf8), "<two words>|<it's fine>|<>")
    }
}
