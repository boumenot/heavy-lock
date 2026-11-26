import Foundation

private struct CommandWord {
    let value: String
    let quoted: Bool
}

private enum ShellToken {
    case word(CommandWord)
    case operatorToken(String)
}

private enum ShellInput {
    case command(Int)
    case script(Int)
}

func fullCommand(_ argv: [String]) -> String {
    let safe = CharacterSet(charactersIn: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_@%+=:,./-")
    return argv.map { argument in
        if !argument.isEmpty, argument.unicodeScalars.allSatisfy({ safe.contains($0) }) {
            return argument
        }
        return "'" + argument.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }.joined(separator: " ")
}

func summarizeCommand(_ argv: [String]) -> String {
    guard let executable = argv.first, !executable.isEmpty else { return "Command unavailable" }
    let program = (executable as NSString).lastPathComponent
    let direct = argv.map { CommandWord(value: $0, quoted: $0.contains(where: \.isWhitespace)) }
    if (program == "bash" || program == "sh"), case let .command(index)? = shellInput(argv),
       let command = firstShellCommand(argv[index]) {
        return summarizeWords(command.words, omitted: command.omitted || index + 1 < argv.count)
    }
    return summarizeWords(direct, omitted: false)
}

private func shellInput(_ arguments: [String]) -> ShellInput? {
    var index = 1
    var command = false
    var standardInput = false
    while index < arguments.count {
        let option = arguments[index]
        if option == "--" || option == "-" {
            index += 1
            break
        }
        guard option.hasPrefix("-") || option.hasPrefix("+") else { break }
        let longName = String(option.dropFirst(option.hasPrefix("--") ? 2 : 1))
        if option.hasPrefix("-"),
           ["noprofile", "norc", "login", "posix", "restricted", "noediting"].contains(longName) {
            index += 1
            continue
        }
        if option.hasPrefix("-"), ["rcfile", "init-file"].contains(longName) {
            index += 1
            guard index < arguments.count else { return nil }
        } else if option.hasPrefix("--") {
            return nil
        } else {
            let flags = option.dropFirst()
            if flags.contains("c") { command = true }
            if flags.contains("s") { standardInput = true }
            for flag in flags where flag == "o" || flag == "O" {
                index += 1
                guard index < arguments.count else { return nil }
            }
        }
        index += 1
    }
    guard index < arguments.count else { return nil }
    if standardInput && !command { return nil }
    return command ? .command(index) : .script(index)
}

private func summarizeWords(_ words: [CommandWord], omitted: Bool) -> String {
    guard let first = words.first, !first.value.isEmpty else { return "Command unavailable" }
    if let script = scriptName(words) { return script }

    let program = singleLine((first.value as NSString).lastPathComponent)
    guard !program.isEmpty else { return "Command unavailable" }
    var parts = [program]
    var shortened = omitted
    for word in words.dropFirst() {
        if word.value.isEmpty || word.quoted || word.value.hasPrefix("-") ||
            (["bash", "sh"].contains(program) && word.value.hasPrefix("+")) ||
            word.value.contains("/") || word.value.contains("=") || parts.count == 4 {
            shortened = true
            break
        }
        parts.append(word.value)
    }
    return parts.joined(separator: " ") + (shortened ? " …" : "")
}

private func scriptName(_ words: [CommandWord]) -> String? {
    let executable = (words[0].value as NSString).lastPathComponent
    let interpreter: String
    var index: Int
    if executable == "fvm", words.count > 1, words[1].value == "dart" {
        interpreter = "dart"
        index = 2
    } else if ["python", "python3", "bash", "sh", "dart", "node"].contains(executable) {
        interpreter = executable
        index = 1
    } else {
        return nil
    }

    if ["bash", "sh"].contains(interpreter) {
        guard case let .script(scriptIndex)? = shellInput(words.map(\.value)) else { return nil }
        index = scriptIndex
    } else {
        let modeFlags = interpreter == "node" ? "ep" :
            (["python", "python3"].contains(interpreter) ? "cm" : "")
        let valueFlags = interpreter == "node" ? "rC" :
            (["python", "python3"].contains(interpreter) ? "WX" : "")
        let shortFlags = interpreter == "node" ? "ci" : "bBdEiIOPqRsSuvx"
        let longValueOptions = interpreter == "node" ?
            ["--require", "--import", "--conditions"] : ["--check-hash-based-pycs"]
        let longSwitchOptions = interpreter == "node" ?
            ["--inspect", "--inspect-brk", "--watch", "--check"] : []
        while index < words.count {
            let value = words[index].value
            if value == "--" {
                index += 1
                break
            }
            if value == "-" { return nil }
            if value == "--eval" || value == "--print" ||
                (modeFlags.isEmpty && ["-c", "-m", "-e", "-p"].contains(value)) {
                return nil
            }
            if !value.hasPrefix("-") { break }
            if !modeFlags.isEmpty, value.hasPrefix("--") {
                let option = String(value.prefix(while: { $0 != "=" }))
                if longValueOptions.contains(option) {
                    if option == value { index += 1 }
                } else if !longSwitchOptions.contains(option) {
                    return nil
                }
            } else if !modeFlags.isEmpty {
                let flags = Array(value.dropFirst())
                for (offset, flag) in flags.enumerated() {
                    if modeFlags.contains(flag) { return nil }
                    if valueFlags.contains(flag) {
                        if offset == flags.count - 1 { index += 1 }
                        break
                    }
                    if !shortFlags.contains(flag) { return nil }
                }
            } else if ["-W", "-X", "-r", "--require"].contains(value) {
                index += 1
            }
            index += 1
        }
    }
    if interpreter == "dart", index < words.count, words[index].value == "run" {
        index += 1
    }
    guard index < words.count else { return nil }
    let path = words[index].value
    guard !path.isEmpty, path != "-", !path.contains("$"), !path.contains("`") else { return nil }
    if interpreter == "dart", !path.hasSuffix(".dart") { return nil }
    let name = singleLine((path as NSString).lastPathComponent)
    return name.isEmpty ? nil : name
}

private func singleLine(_ value: String) -> String {
    String(value.map { $0.isWhitespace ? " " : $0 }).trimmingCharacters(in: .whitespaces)
}

private func firstShellCommand(_ script: String) -> (words: [CommandWord], omitted: Bool)? {
    guard let tokens = tokenizeShell(script) else { return nil }
    var index = 0
    var skippedDirectory = false
    while index < tokens.count {
        if case .operatorToken(let operation) = tokens[index], operation == "(" || operation == "\n" {
            index += 1
            continue
        }
        if case .word(let word) = tokens[index], word.value == "cd" {
            var next = index + 1
            while next < tokens.count {
                if case .operatorToken = tokens[next] { break }
                next += 1
            }
            if next < tokens.count, case .operatorToken("&&") = tokens[next] {
                skippedDirectory = true
                index = next + 1
                continue
            }
        }
        break
    }
    var words: [CommandWord] = []
    while index < tokens.count {
        guard case .word(let word) = tokens[index] else { break }
        words.append(word)
        index += 1
    }
    guard !words.isEmpty else { return nil }
    let hasFollowingCommand = tokens[index...].contains { token in
        if case .operatorToken(let operation) = token, operation == ")" || operation == "\n" { return false }
        return true
    }
    return (words, skippedDirectory || hasFollowingCommand)
}

private func tokenizeShell(_ script: String) -> [ShellToken]? {
    let characters = Array(script)
    var tokens: [ShellToken] = []
    var value = ""
    var quoted = false
    var hasWord = false
    var quote: Character?
    var index = 0

    func finishWord() {
        if hasWord {
            tokens.append(.word(CommandWord(value: value, quoted: quoted)))
            value = ""
            quoted = false
            hasWord = false
        }
    }

    while index < characters.count {
        let character = characters[index]
        if let currentQuote = quote {
            if character == currentQuote {
                quote = nil
            } else if character == "\\", currentQuote == "\"" {
                index += 1
                guard index < characters.count else { return nil }
                if !characters[index].isNewline { value.append(characters[index]) }
            } else {
                value.append(character)
            }
        } else if character == "'" || character == "\"" {
            quote = character
            quoted = true
            hasWord = true
        } else if character == "\\" {
            index += 1
            guard index < characters.count else { return nil }
            if !characters[index].isNewline {
                value.append(characters[index])
                quoted = true
                hasWord = true
            }
        } else if character.isNewline {
            finishWord()
            tokens.append(.operatorToken("\n"))
        } else if character.isWhitespace {
            finishWord()
        } else if "();&|<>".contains(character) {
            if (character == "<" || character == ">"), hasWord, !quoted, !value.isEmpty,
               value.utf8.allSatisfy({ (48...57).contains($0) }) {
                value = ""
                hasWord = false
            } else {
                finishWord()
            }
            var operation = String(character)
            if (character == "&" || character == "|"), index + 1 < characters.count,
               characters[index + 1] == character {
                operation.append(character)
                index += 1
            }
            tokens.append(.operatorToken(operation))
        } else {
            value.append(character)
            hasWord = true
        }
        index += 1
    }
    guard quote == nil else { return nil }
    finishWord()
    return tokens
}
