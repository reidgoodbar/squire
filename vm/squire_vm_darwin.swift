import Darwin
import Dispatch
import Foundation
import Virtualization

enum SquireVMError: Error, CustomStringConvertible {
    case usage(String)
    case unavailable(String)
    case timeout(String)
    case protocolError(String)

    var description: String {
        switch self {
        case .usage(let message), .unavailable(let message), .timeout(let message), .protocolError(let message):
            return message
        }
    }
}

struct GuestRequest: Codable {
    let version: Int
    let cwd: String
    let storeRoot: String
    let argv: [String]
    let env: [String: String]
    let interactive: Bool
    let terminalRows: Int
    let terminalCols: Int

    enum CodingKeys: String, CodingKey {
        case version
        case cwd
        case storeRoot = "store_root"
        case argv
        case env
        case interactive
        case terminalRows = "terminal_rows"
        case terminalCols = "terminal_cols"
    }
}

struct GuestResponse: Codable {
    let stdoutB64: String?
    let stderrB64: String?
    let exitCode: Int32

    enum CodingKeys: String, CodingKey {
        case stdoutB64 = "stdout_b64"
        case stderrB64 = "stderr_b64"
        case exitCode = "exit_code"
    }
}

struct VMStatus: Codable {
    let productMode: String
    let backend: String
    let available: Bool
    let frameworkSupported: Bool
    let guestConfigured: Bool
    let guestOS: String
    let usesHostCommandShims: Bool
    let diagnostics: [String]

    enum CodingKeys: String, CodingKey {
        case productMode = "product_mode"
        case backend
        case available
        case frameworkSupported = "framework_supported"
        case guestConfigured = "guest_configured"
        case guestOS = "guest_os"
        case usesHostCommandShims = "uses_host_command_shims"
        case diagnostics
    }
}

struct GuestConfig {
    let kernelURL: URL
    let initrdURL: URL?
    let diskURL: URL?
    let memoryBytes: UInt64
    let cpuCount: Int
    let agentPort: UInt32
    let bootTimeoutSeconds: Double
    let workspaceTag: String
    let storeTag: String
    let codexHomeTag: String
    let codexHomeURL: URL?
    let kernelCommandLine: String
    let transport: String
    let networkEnabled: Bool
}

struct SessionOptions {
    let cwd: String
    let storeRoot: String
    let command: [String]
}

@main
struct SquireVMDarwin {
    static func main() {
        do {
            try run(Array(CommandLine.arguments.dropFirst()))
        } catch {
            let message = String(describing: error)
            FileHandle.standardError.write(Data("squire-vm-darwin: \(message)\n".utf8))
            exit(1)
        }
    }

    static func run(_ args: [String]) throws {
        guard let subcommand = args.first else {
            throw SquireVMError.usage("missing subcommand: status or session")
        }
        switch subcommand {
        case "status":
            try printStatus(args: Array(args.dropFirst()))
        case "session":
            let opts = try parseSession(Array(args.dropFirst()))
            let code = try runSession(opts)
            exit(code)
        default:
            throw SquireVMError.usage("unknown subcommand \(subcommand)")
        }
    }

    static func printStatus(args: [String]) throws {
        let json = args.contains("--json")
        let configResult = loadGuestConfig()
        var diagnostics: [String] = []
        let configured: Bool
        switch configResult {
        case .success:
            configured = true
            diagnostics.append("guest kernel configuration is present")
        case .failure(let error):
            configured = false
            diagnostics.append(error.description)
        }
        let supported = VZVirtualMachine.isSupported
        if !supported {
            diagnostics.append("Virtualization.framework reports this host is unsupported")
        }
        let status = VMStatus(
            productMode: "linux_guest_session",
            backend: "virtualization-framework",
            available: configured && supported,
            frameworkSupported: supported,
            guestConfigured: configured,
            guestOS: "linux",
            usesHostCommandShims: false,
            diagnostics: diagnostics
        )
        if json {
            let data = try JSONEncoder().encode(status)
            FileHandle.standardOutput.write(data)
            FileHandle.standardOutput.write(Data("\n".utf8))
            return
        }
        FileHandle.standardOutput.write(Data("""
        Squire VM Darwin helper
        backend: virtualization-framework
        available: \(status.available)
        framework_supported: \(status.frameworkSupported)
        guest_configured: \(status.guestConfigured)
        uses_host_command_shims: false

        """.utf8))
        for diagnostic in diagnostics {
            FileHandle.standardOutput.write(Data("diagnostic: \(diagnostic)\n".utf8))
        }
    }

    static func parseSession(_ args: [String]) throws -> SessionOptions {
        var cwd: String?
        var storeRoot: String?
        var i = 0
        while i < args.count {
            switch args[i] {
            case "--cwd":
                i += 1
                guard i < args.count else { throw SquireVMError.usage("session --cwd requires a value") }
                cwd = args[i]
            case "--store-root":
                i += 1
                guard i < args.count else { throw SquireVMError.usage("session --store-root requires a value") }
                storeRoot = args[i]
            case "--":
                let command = Array(args.dropFirst(i + 1))
                guard !command.isEmpty else { throw SquireVMError.usage("session requires a command after --") }
                guard let cwd = cwd else { throw SquireVMError.usage("session requires --cwd") }
                guard let storeRoot = storeRoot else { throw SquireVMError.usage("session requires --store-root") }
                return SessionOptions(cwd: cwd, storeRoot: storeRoot, command: command)
            default:
                throw SquireVMError.usage("unknown session option \(args[i])")
            }
            i += 1
        }
        throw SquireVMError.usage("session requires -- before the command")
    }

    static func runSession(_ opts: SessionOptions) throws -> Int32 {
        guard VZVirtualMachine.isSupported else {
            throw SquireVMError.unavailable("Virtualization.framework is not supported on this host")
        }
        let guest = try loadGuestConfig().get()
        let hostToGuest = Pipe()
        let guestToHost = Pipe()
        let interactive = shouldRunInteractive(opts.command)
        let interactiveHostToGuest = interactive ? Pipe() : nil
        let interactiveGuestToHost = interactive ? Pipe() : nil
        let vm = try buildVM(
            config: guest,
            workspace: opts.cwd,
            storeRoot: opts.storeRoot,
            serialRead: hostToGuest.fileHandleForReading,
            serialWrite: guestToHost.fileHandleForWriting,
            interactiveSerialRead: interactiveHostToGuest?.fileHandleForReading,
            interactiveSerialWrite: interactiveGuestToHost?.fileHandleForWriting
        )
        defer {
            stopVM(vm)
        }
        try startVM(vm, timeoutSeconds: guest.bootTimeoutSeconds)
        let terminalSize = interactive ? readTerminalSize() : TerminalSize(rows: 0, cols: 0)
        let request = GuestRequest(
            version: 1,
            cwd: "/mnt/squire-workspace",
            storeRoot: "/mnt/squire-store",
            argv: opts.command,
            env: guestForwardedEnvironment(),
            interactive: interactive,
            terminalRows: terminalSize.rows,
            terminalCols: terminalSize.cols
        )
        let response: GuestResponse
        if guest.transport == "vsock" {
            if interactive {
                throw SquireVMError.unavailable("interactive VM sessions require serial transport")
            }
            guard let socket = firstVirtioSocket(vm) else {
                throw SquireVMError.unavailable("guest did not expose a virtio socket device")
            }
            let connection = try connect(socket, port: guest.agentPort, timeoutSeconds: guest.bootTimeoutSeconds)
            defer {
                connection.close()
            }
            response = try sendVSockRequest(connection: connection, request: request)
        } else if interactive {
            guard let interactiveHostToGuest = interactiveHostToGuest, let interactiveGuestToHost = interactiveGuestToHost else {
                throw SquireVMError.protocolError("interactive serial pipes were not configured")
            }
            response = try sendInteractiveSerialRequest(
                controlWriter: hostToGuest.fileHandleForWriting,
                controlReader: guestToHost.fileHandleForReading,
                interactiveWriter: interactiveHostToGuest.fileHandleForWriting,
                interactiveReader: interactiveGuestToHost.fileHandleForReading,
                timeoutSeconds: guest.bootTimeoutSeconds,
                request: request
            )
        } else {
            response = try sendSerialRequest(
                writer: hostToGuest.fileHandleForWriting,
                reader: guestToHost.fileHandleForReading,
                timeoutSeconds: guest.bootTimeoutSeconds,
                request: request
            )
        }
        guard let stdout = Data(base64Encoded: response.stdoutB64 ?? "") else {
            throw SquireVMError.protocolError("guest response stdout_b64 is not valid base64")
        }
        guard let stderr = Data(base64Encoded: response.stderrB64 ?? "") else {
            throw SquireVMError.protocolError("guest response stderr_b64 is not valid base64")
        }
        FileHandle.standardOutput.write(stdout)
        FileHandle.standardError.write(stderr)
        return response.exitCode
    }

    static func shouldRunInteractive(_ command: [String]) -> Bool {
        let env = ProcessInfo.processInfo.environment
        if env["SQUIRE_VM_INTERACTIVE"] == "1" {
            return true
        }
        if env["SQUIRE_VM_INTERACTIVE"] == "0" {
            return false
        }
        guard Darwin.isatty(STDIN_FILENO) == 1 else {
            return false
        }
        guard command.count == 1 else {
            return false
        }
        return URL(fileURLWithPath: command[0]).lastPathComponent == "codex"
    }

    struct TerminalSize {
        let rows: Int
        let cols: Int
    }

    static func guestForwardedEnvironment() -> [String: String] {
        let env = ProcessInfo.processInfo.environment
        let allowed = [
            "SQUIRE_VM_GUEST_SESSION_TRANSPORT",
            "SQUIRE_VM_GUEST_PRELOAD_LIB",
            "SQUIRE_PRELOAD_ENABLE",
            "SQUIRE_PRELOAD_LIB",
            "SQUIRE_PRELOAD_HELPER",
            "SQUIRE_PRELOAD_TRACE",
            "SQUIRE_SHIM_DEBUG",
            "SQUIRE_SHIM_REQUIRE_HIT",
            "SQUIRE_SHIM_DISABLE_EVENT_LOG",
            "SQUIRE_SHIM_ENABLE_WARM_FILE_REPLAY",
        ]
        var out: [String: String] = [:]
        for key in allowed {
            if let value = env[key], !value.isEmpty {
                out[key] = value
            }
        }
        return out
    }

    static func readTerminalSize() -> TerminalSize {
        var size = winsize()
        if Darwin.ioctl(STDIN_FILENO, TIOCGWINSZ, &size) == 0 {
            let rows = Int(size.ws_row)
            let cols = Int(size.ws_col)
            if rows > 0 && cols > 0 {
                return TerminalSize(rows: rows, cols: cols)
            }
        }
        let env = ProcessInfo.processInfo.environment
        let rows = Int(env["LINES"] ?? "") ?? 24
        let cols = Int(env["COLUMNS"] ?? "") ?? 80
        return TerminalSize(rows: max(rows, 1), cols: max(cols, 20))
    }

    static func loadGuestConfig() -> Result<GuestConfig, SquireVMError> {
        let env = ProcessInfo.processInfo.environment
        let bundle = env["SQUIRE_VM_BUNDLE"].map { URL(fileURLWithPath: $0) }
        let kernel = envPath("SQUIRE_VM_KERNEL", fallback: bundle?.appendingPathComponent("kernel").path)
        guard let kernelPath = kernel, FileManager.default.isReadableFile(atPath: kernelPath) else {
            return .failure(.unavailable("missing readable SQUIRE_VM_KERNEL or SQUIRE_VM_BUNDLE/kernel"))
        }
        let initrdPath = envPath("SQUIRE_VM_INITRD", fallback: bundle?.appendingPathComponent("initrd").path)
        let diskPath = envPath("SQUIRE_VM_DISK", fallback: bundle?.appendingPathComponent("disk.img").path)
        let initrd = readableOptionalFile(initrdPath)
        let disk = readableOptionalFile(diskPath)
        guard initrd != nil || disk != nil else {
            return .failure(.unavailable("missing readable SQUIRE_VM_INITRD or SQUIRE_VM_DISK"))
        }
        let memoryMB = UInt64(env["SQUIRE_VM_MEMORY_MB"] ?? "") ?? 2048
        let cpus = Int(env["SQUIRE_VM_CPUS"] ?? "") ?? 2
        let port: UInt32
        if let rawPort = env["SQUIRE_VM_AGENT_PORT"], !rawPort.isEmpty {
            guard let parsedPort = UInt32(rawPort), parsedPort > 0 else {
                return .failure(.unavailable("SQUIRE_VM_AGENT_PORT must be a positive uint32"))
            }
            port = parsedPort
        } else {
            port = 1024
        }
        let timeout = Double(env["SQUIRE_VM_BOOT_TIMEOUT_SECONDS"] ?? "") ?? 20.0
        let transport = env["SQUIRE_VM_TRANSPORT"] ?? "serial"
        guard transport == "serial" || transport == "vsock" else {
            return .failure(.unavailable("SQUIRE_VM_TRANSPORT must be serial or vsock"))
        }
        var codexHomeURL: URL?
        if let rawCodexHome = env["SQUIRE_VM_CODEX_HOME"], !rawCodexHome.isEmpty {
            var isDir: ObjCBool = false
            guard FileManager.default.fileExists(atPath: rawCodexHome, isDirectory: &isDir), isDir.boolValue else {
                return .failure(.unavailable("SQUIRE_VM_CODEX_HOME must point to a readable directory"))
            }
            codexHomeURL = URL(fileURLWithPath: rawCodexHome)
        }
        let networkEnabled = env["SQUIRE_VM_NETWORK"] != "0"
        let defaultKernelArgs = transport == "serial" ? "console=hvc0 quiet" : "console=hvc0"
        let cmdline = env["SQUIRE_VM_KERNEL_ARGS"] ?? defaultKernelArgs
        return .success(GuestConfig(
            kernelURL: URL(fileURLWithPath: kernelPath),
            initrdURL: initrd.map { URL(fileURLWithPath: $0) },
            diskURL: disk.map { URL(fileURLWithPath: $0) },
            memoryBytes: memoryMB * 1024 * 1024,
            cpuCount: cpus,
            agentPort: port,
            bootTimeoutSeconds: timeout,
            workspaceTag: env["SQUIRE_VM_WORKSPACE_TAG"] ?? "squire-workspace",
            storeTag: env["SQUIRE_VM_STORE_TAG"] ?? "squire-store",
            codexHomeTag: env["SQUIRE_VM_CODEX_HOME_TAG"] ?? "squire-codex-home",
            codexHomeURL: codexHomeURL,
            kernelCommandLine: cmdline,
            transport: transport,
            networkEnabled: networkEnabled
        ))
    }

    static func envPath(_ key: String, fallback: String?) -> String? {
        if let value = ProcessInfo.processInfo.environment[key], !value.isEmpty {
            return value
        }
        return fallback
    }

    static func readableOptionalFile(_ path: String?) -> String? {
        guard let path = path, FileManager.default.isReadableFile(atPath: path) else {
            return nil
        }
        return path
    }

    static func buildVM(
        config guest: GuestConfig,
        workspace: String,
        storeRoot: String,
        serialRead: FileHandle,
        serialWrite: FileHandle,
        interactiveSerialRead: FileHandle?,
        interactiveSerialWrite: FileHandle?
    ) throws -> VZVirtualMachine {
        try FileManager.default.createDirectory(at: URL(fileURLWithPath: storeRoot), withIntermediateDirectories: true)

        let bootLoader = VZLinuxBootLoader(kernelURL: guest.kernelURL)
        bootLoader.initialRamdiskURL = guest.initrdURL
        bootLoader.commandLine = guest.kernelCommandLine

        let platform = VZGenericPlatformConfiguration()
        platform.machineIdentifier = VZGenericMachineIdentifier()

        let config = VZVirtualMachineConfiguration()
        config.platform = platform
        config.bootLoader = bootLoader
        config.memorySize = max(guest.memoryBytes, VZVirtualMachineConfiguration.minimumAllowedMemorySize)
        config.cpuCount = min(max(guest.cpuCount, VZVirtualMachineConfiguration.minimumAllowedCPUCount), VZVirtualMachineConfiguration.maximumAllowedCPUCount)
        config.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
        config.socketDevices = [VZVirtioSocketDeviceConfiguration()]
        var serialPorts = [makeSerialPort(reading: serialRead, writing: serialWrite)]
        if let interactiveSerialRead = interactiveSerialRead, let interactiveSerialWrite = interactiveSerialWrite {
            serialPorts.append(makeSerialPort(reading: interactiveSerialRead, writing: interactiveSerialWrite))
        }
        config.serialPorts = serialPorts
        if guest.networkEnabled {
            let network = VZVirtioNetworkDeviceConfiguration()
            network.attachment = VZNATNetworkDeviceAttachment()
            config.networkDevices = [network]
        }

        if let diskURL = guest.diskURL {
            let attachment = try VZDiskImageStorageDeviceAttachment(url: diskURL, readOnly: false)
            config.storageDevices = [VZVirtioBlockDeviceConfiguration(attachment: attachment)]
        }
        var shares = [
            makeFileSystemDevice(tag: guest.workspaceTag, path: workspace, readOnly: false),
            makeFileSystemDevice(tag: guest.storeTag, path: storeRoot, readOnly: false),
        ]
        if let codexHomeURL = guest.codexHomeURL {
            shares.append(makeFileSystemDevice(tag: guest.codexHomeTag, path: codexHomeURL.path, readOnly: false))
        }
        config.directorySharingDevices = shares

        try config.validate()
        let queue = DispatchQueue(label: "run.squire.vm.darwin")
        return VZVirtualMachine(configuration: config, queue: queue)
    }

    static func makeSerialPort(reading: FileHandle, writing: FileHandle) -> VZVirtioConsoleDeviceSerialPortConfiguration {
        let serial = VZVirtioConsoleDeviceSerialPortConfiguration()
        serial.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: reading, fileHandleForWriting: writing)
        return serial
    }

    static func makeFileSystemDevice(tag: String, path: String, readOnly: Bool) -> VZVirtioFileSystemDeviceConfiguration {
        let device = VZVirtioFileSystemDeviceConfiguration(tag: tag)
        let directory = VZSharedDirectory(url: URL(fileURLWithPath: path), readOnly: readOnly)
        device.share = VZSingleDirectoryShare(directory: directory)
        return device
    }

    static func startVM(_ vm: VZVirtualMachine, timeoutSeconds: Double) throws {
        let semaphore = DispatchSemaphore(value: 0)
        var startError: Error?
        vm.queue.async {
            vm.start { result in
                if case .failure(let error) = result {
                    startError = error
                }
                semaphore.signal()
            }
        }
        if semaphore.wait(timeout: .now() + timeoutSeconds) == .timedOut {
            throw SquireVMError.timeout("timed out starting Linux guest")
        }
        if let startError = startError {
            throw startError
        }
    }

    static func firstVirtioSocket(_ vm: VZVirtualMachine) -> VZVirtioSocketDevice? {
        let semaphore = DispatchSemaphore(value: 0)
        var device: VZVirtioSocketDevice?
        vm.queue.async {
            device = vm.socketDevices.compactMap { $0 as? VZVirtioSocketDevice }.first
            semaphore.signal()
        }
        _ = semaphore.wait(timeout: .now() + 5)
        return device
    }

    static func connect(_ socket: VZVirtioSocketDevice, port: UInt32, timeoutSeconds: Double) throws -> VZVirtioSocketConnection {
        let deadline = Date().addingTimeInterval(timeoutSeconds)
        var lastError: Error?
        while Date() < deadline {
            let semaphore = DispatchSemaphore(value: 0)
            var connection: VZVirtioSocketConnection?
            socket.connect(toPort: port) { result in
                switch result {
                case .success(let connected):
                    connection = connected
                case .failure(let error):
                    lastError = error
                }
                semaphore.signal()
            }
            _ = semaphore.wait(timeout: .now() + 1)
            if let connection = connection {
                return connection
            }
            Thread.sleep(forTimeInterval: 0.1)
        }
        if let lastError = lastError {
            throw lastError
        }
        throw SquireVMError.timeout("timed out connecting to guest agent on vsock port \(port)")
    }

    static func sendVSockRequest(connection: VZVirtioSocketConnection, request: GuestRequest) throws -> GuestResponse {
        let fd = connection.fileDescriptor
        guard fd >= 0 else {
            throw SquireVMError.protocolError("vsock connection is closed")
        }
        var data = try JSONEncoder().encode(request)
        data.append(0x0A)
        try writeAll(fd: fd, data: data)
        let responseData = try readAll(fd: fd, maxBytes: 64 * 1024 * 1024)
        return try JSONDecoder().decode(GuestResponse.self, from: responseData)
    }

    static func sendSerialRequest(writer: FileHandle, reader: FileHandle, timeoutSeconds: Double, request: GuestRequest) throws -> GuestResponse {
        let deadline = Date().addingTimeInterval(timeoutSeconds)
        let debugSerial = ProcessInfo.processInfo.environment["SQUIRE_VM_DEBUG_SERIAL"] == "1"
        try waitForSerialReady(fd: reader.fileDescriptor, deadline: deadline, debugSerial: debugSerial)
        var data = try JSONEncoder().encode(request)
        data.append(0x0A)
        try writeAll(fd: writer.fileDescriptor, data: data)
        var lastDecodeError: Error?
        while Date() < deadline {
            let line = try readLine(fd: reader.fileDescriptor, maxBytes: 64 * 1024 * 1024, deadline: deadline)
            let trimmed = line.trimmingPrefix { $0 == 0x20 || $0 == 0x09 || $0 == 0x0D || $0 == 0x0A }
            guard trimmed.first == 0x7B else {
                if debugSerial {
                    FileHandle.standardError.write(line)
                }
                continue
            }
            do {
                return try JSONDecoder().decode(GuestResponse.self, from: trimmed)
            } catch {
                lastDecodeError = error
                if debugSerial {
                    FileHandle.standardError.write(line)
                }
            }
        }
        if let lastDecodeError = lastDecodeError {
            throw lastDecodeError
        }
        throw SquireVMError.timeout("timed out waiting for guest serial response")
    }

    static func sendInteractiveSerialRequest(
        controlWriter: FileHandle,
        controlReader: FileHandle,
        interactiveWriter: FileHandle,
        interactiveReader: FileHandle,
        timeoutSeconds: Double,
        request: GuestRequest
    ) throws -> GuestResponse {
        let deadline = Date().addingTimeInterval(timeoutSeconds)
        let debugSerial = ProcessInfo.processInfo.environment["SQUIRE_VM_DEBUG_SERIAL"] == "1"
        try waitForSerialReady(fd: controlReader.fileDescriptor, deadline: deadline, debugSerial: debugSerial)
        var data = try JSONEncoder().encode(request)
        data.append(0x0A)
        try writeAll(fd: controlWriter.fileDescriptor, data: data)
        return try withRawTerminal {
            bridgeInteractiveIO(
                inputFD: STDIN_FILENO,
                outputFD: STDOUT_FILENO,
                guestInputFD: interactiveWriter.fileDescriptor,
                guestOutputFD: interactiveReader.fileDescriptor
            )
            while true {
                let line = try readLineBlocking(fd: controlReader.fileDescriptor, maxBytes: 1024 * 1024)
                let trimmed = line.trimmingPrefix { $0 == 0x20 || $0 == 0x09 || $0 == 0x0D || $0 == 0x0A }
                guard trimmed.first == 0x7B else {
                    if debugSerial {
                        FileHandle.standardError.write(line)
                    }
                    continue
                }
                do {
                    return try JSONDecoder().decode(GuestResponse.self, from: trimmed)
                } catch {
                    if debugSerial {
                        FileHandle.standardError.write(line)
                    }
                    if ProcessInfo.processInfo.environment["SQUIRE_VM_STRICT_INTERACTIVE_DECODE"] == "1" {
                        throw error
                    }
                }
            }
        }
    }

    static func bridgeInteractiveIO(inputFD: Int32, outputFD: Int32, guestInputFD: Int32, guestOutputFD: Int32) {
        DispatchQueue.global(qos: .userInitiated).async {
            copyFD(from: inputFD, to: guestInputFD)
        }
        DispatchQueue.global(qos: .userInitiated).async {
            copyFD(from: guestOutputFD, to: outputFD)
        }
    }

    static func copyFD(from inputFD: Int32, to outputFD: Int32) {
        var buffer = [UInt8](repeating: 0, count: 16 * 1024)
        while true {
            let readCount = buffer.withUnsafeMutableBytes { rawBuffer -> Int in
                guard let base = rawBuffer.baseAddress else { return 0 }
                return Darwin.read(inputFD, base, rawBuffer.count)
            }
            if readCount < 0 {
                if errno == EINTR { continue }
                return
            }
            if readCount == 0 {
                return
            }
            var writeFailed = false
            buffer.withUnsafeBytes { rawBuffer in
                guard let base = rawBuffer.baseAddress else { return }
                var offset = 0
                while offset < readCount {
                    let wrote = Darwin.write(outputFD, base.advanced(by: offset), readCount - offset)
                    if wrote < 0 {
                        if errno == EINTR { continue }
                        writeFailed = true
                        return
                    }
                    if wrote == 0 {
                        writeFailed = true
                        return
                    }
                    offset += wrote
                }
            }
            if writeFailed {
                return
            }
        }
    }

    static func withRawTerminal<T>(_ body: () throws -> T) throws -> T {
        guard Darwin.isatty(STDIN_FILENO) == 1 else {
            return try body()
        }
        var original = termios()
        guard tcgetattr(STDIN_FILENO, &original) == 0 else {
            return try body()
        }
        var raw = original
        cfmakeraw(&raw)
        if tcsetattr(STDIN_FILENO, TCSANOW, &raw) != 0 {
            return try body()
        }
        defer {
            var restore = original
            _ = tcsetattr(STDIN_FILENO, TCSANOW, &restore)
        }
        return try body()
    }

    static func waitForSerialReady(fd: Int32, deadline: Date, debugSerial: Bool) throws {
        while Date() < deadline {
            let line = try readLine(fd: fd, maxBytes: 1024 * 1024, deadline: deadline)
            if line.contains(Data("SQUIRE_VM_AGENT_READY".utf8)) {
                return
            }
            if debugSerial {
                FileHandle.standardError.write(line)
            }
        }
        throw SquireVMError.timeout("timed out waiting for guest agent readiness")
    }

    static func writeAll(fd: Int32, data: Data) throws {
        try data.withUnsafeBytes { buffer in
            guard let base = buffer.baseAddress else { return }
            var offset = 0
            while offset < buffer.count {
                let wrote = Darwin.write(fd, base.advanced(by: offset), buffer.count - offset)
                if wrote < 0 {
                    if errno == EINTR { continue }
                    throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
                }
                if wrote == 0 {
                    throw SquireVMError.protocolError("vsock write returned zero bytes")
                }
                offset += wrote
            }
        }
    }

    static func readAll(fd: Int32, maxBytes: Int) throws -> Data {
        var out = Data()
        var buffer = [UInt8](repeating: 0, count: 32 * 1024)
        while true {
            let readCount = Darwin.read(fd, &buffer, buffer.count)
            if readCount < 0 {
                if errno == EINTR { continue }
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
            }
            if readCount == 0 {
                return out
            }
            out.append(buffer, count: readCount)
            if out.count > maxBytes {
                throw SquireVMError.protocolError("guest response exceeded \(maxBytes) bytes")
            }
        }
    }

    static func readLine(fd: Int32, maxBytes: Int, deadline: Date) throws -> Data {
        var out = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while Date() < deadline {
            var pollFD = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
            let remainingMS = max(1, min(100, Int(deadline.timeIntervalSinceNow * 1000)))
            let ready = Darwin.poll(&pollFD, 1, Int32(remainingMS))
            if ready < 0 {
                if errno == EINTR { continue }
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
            }
            if ready == 0 {
                continue
            }
            let readCount = Darwin.read(fd, &buffer, buffer.count)
            if readCount < 0 {
                if errno == EINTR { continue }
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
            }
            if readCount == 0 {
                continue
            }
            out.append(buffer, count: readCount)
            if out.count > maxBytes {
                throw SquireVMError.protocolError("guest response exceeded \(maxBytes) bytes")
            }
            if out.contains(0x0A) {
                return out
            }
        }
        throw SquireVMError.timeout("timed out waiting for guest serial line")
    }

    static func readLineBlocking(fd: Int32, maxBytes: Int) throws -> Data {
        var out = Data()
        var buffer = [UInt8](repeating: 0, count: 4096)
        while true {
            let readCount = Darwin.read(fd, &buffer, buffer.count)
            if readCount < 0 {
                if errno == EINTR { continue }
                throw NSError(domain: NSPOSIXErrorDomain, code: Int(errno))
            }
            if readCount == 0 {
                throw SquireVMError.protocolError("guest control serial closed before exit response")
            }
            out.append(buffer, count: readCount)
            if out.count > maxBytes {
                throw SquireVMError.protocolError("guest response exceeded \(maxBytes) bytes")
            }
            if out.contains(0x0A) {
                return out
            }
        }
    }

    static func stopVM(_ vm: VZVirtualMachine) {
        let semaphore = DispatchSemaphore(value: 0)
        vm.queue.async {
            if vm.canRequestStop {
                do {
                    try vm.requestStop()
                } catch {
                    // Fall through to destructive stop below when the guest refuses.
                }
            }
            if vm.canStop {
                vm.stop { _ in
                    semaphore.signal()
                }
            } else {
                semaphore.signal()
            }
        }
        _ = semaphore.wait(timeout: .now() + 5)
    }
}
