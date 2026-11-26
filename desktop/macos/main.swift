import Cocoa
import WebKit

final class AppDelegate: NSObject, NSApplicationDelegate, WKUIDelegate, WKNavigationDelegate, WKDownloadDelegate {
    private var window: NSWindow!
    private var webView: WKWebView!
    private var serverProcess: Process?
    private var startedByApp = false

    private let appDir = "/Users/thiagomacbookpro/ArtexV2"
    private let serverAddr = "127.0.0.1:8797"
    private let proxyAddr = "127.0.0.1:8798"
    private var baseURL: URL { URL(string: "http://\(serverAddr)/")! }
    private var healthURL: URL { URL(string: "http://\(serverAddr)/api/health")! }

    func applicationDidFinishLaunching(_ notification: Notification) {
        buildMenus()
        setupWindow()
        loadSplashHTML(message: "Iniciando ArtexV2...")

        DispatchQueue.global(qos: .userInitiated).async {
            self.ensureBackendRunning { ok, errMsg in
                DispatchQueue.main.async {
                    if ok {
                        self.webView.load(URLRequest(url: self.baseURL))
                    } else {
                        self.loadSplashHTML(message: "Falha ao iniciar o servidor ArtexV2:\n\(errMsg ?? "erro desconhecido")")
                    }
                }
            }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        return true
    }

    func applicationWillTerminate(_ notification: Notification) {
        guard startedByApp, let proc = serverProcess, proc.isRunning else { return }
        proc.terminate()
        let deadline = Date().addingTimeInterval(3.0)
        while proc.isRunning && Date() < deadline {
            Thread.sleep(forTimeInterval: 0.05)
        }
        if proc.isRunning {
            kill(proc.processIdentifier, SIGKILL)
        }
    }

    private func setupWindow() {
        let screenFrame = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
        let width = min(1440.0, screenFrame.width * 0.9)
        let height = min(920.0, screenFrame.height * 0.9)
        let rect = NSRect(
            x: screenFrame.midX - width / 2,
            y: screenFrame.midY - height / 2,
            width: width,
            height: height
        )

        window = NSWindow(
            contentRect: rect,
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.title = "Artex"
        window.minSize = NSSize(width: 960, height: 640)
        window.backgroundColor = NSColor(calibratedRed: 0.06, green: 0.06, blue: 0.08, alpha: 1.0)
        window.setFrameAutosaveName("ArtexMainWindow")

        let config = WKWebViewConfiguration()
        config.websiteDataStore = .default()
        config.preferences.setValue(true, forKey: "developerExtrasEnabled")

        webView = WKWebView(frame: window.contentView!.bounds, configuration: config)
        webView.autoresizingMask = [.width, .height]
        webView.uiDelegate = self
        webView.navigationDelegate = self
        if #available(macOS 13.3, *) {
            webView.isInspectable = true
        }

        window.contentView?.addSubview(webView)
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    private func loadSplashHTML(message: String) {
        let escaped = message
            .replacingOccurrences(of: "&", with: "&amp;")
            .replacingOccurrences(of: "<", with: "&lt;")
            .replacingOccurrences(of: ">", with: "&gt;")
            .replacingOccurrences(of: "\n", with: "<br/>")
        let html = """
        <!doctype html>
        <html>
        <head><meta charset="utf-8"><style>
          body { margin:0; height:100vh; display:flex; align-items:center; justify-content:center;
                 background:#0d0e12; color:#e4e4e7; font-family:-apple-system,BlinkMacSystemFont,sans-serif; }
          .box { text-align:center; padding:32px; max-width:520px; }
          .spinner { width:28px; height:28px; margin:0 auto 16px; border:3px solid #27272a;
                     border-top-color:#f97316; border-radius:50%; animation:spin 0.8s linear infinite; }
          @keyframes spin { to { transform:rotate(360deg); } }
          p { font-size:14px; line-height:1.5; color:#a1a1aa; }
        </style></head>
        <body><div class="box"><div class="spinner"></div><p>\(escaped)</p></div></body>
        </html>
        """
        webView.loadHTMLString(html, baseURL: nil)
    }

    // MARK: - Backend lifecycle

    private func enrichedEnvironment() -> [String: String] {
        var env = ProcessInfo.processInfo.environment
        let home = NSHomeDirectory()
        let extraPaths = [
            "\(home)/.local/go/bin",
            "\(home)/.local/bin",
            "/opt/homebrew/opt/postgresql@17/bin",
            "/opt/homebrew/bin",
            "/opt/homebrew/sbin",
            "/usr/local/bin",
            "\(home)/.cargo/bin",
            "/usr/bin",
            "/bin",
            "/usr/sbin",
            "/sbin",
        ]
        let existing = env["PATH"] ?? ""
        var parts = extraPaths
        for p in existing.split(separator: ":").map(String.init) where !parts.contains(p) {
            parts.append(p)
        }
        env["PATH"] = parts.joined(separator: ":")
        env["HOME"] = home
        if env["ARTEX_PG_DSN"] == nil || env["ARTEX_PG_DSN"]!.isEmpty {
            let user = NSUserName()
            env["ARTEX_PG_DSN"] = "postgres://\(user)@127.0.0.1:5432/artex?sslmode=disable"
        }
        // Local desktop app binds strictly to 127.0.0.1 (same-origin + anti-CSRF still enforced).
        env["ARTEX_ALLOW_LOCAL_BYPASS"] = "1"
        return env
    }

    private func isHealthy() -> Bool {
        var req = URLRequest(url: healthURL)
        req.timeoutInterval = 1.0
        let sem = DispatchSemaphore(value: 0)
        var ok = false
        URLSession.shared.dataTask(with: req) { _, resp, err in
            if err == nil, let http = resp as? HTTPURLResponse, http.statusCode == 200 {
                ok = true
            }
            sem.signal()
        }.resume()
        _ = sem.wait(timeout: .now() + 1.5)
        return ok
    }

    private func ensurePostgresRunning(env: [String: String]) {
        let check = Process()
        check.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        check.arguments = ["pg_isready", "-h", "127.0.0.1", "-p", "5432"]
        check.environment = env
        check.standardOutput = Pipe()
        check.standardError = Pipe()
        if (try? check.run()) != nil {
            check.waitUntilExit()
            if check.terminationStatus == 0 {
                return
            }
        }
        let start = Process()
        start.executableURL = URL(fileURLWithPath: "/usr/bin/env")
        start.arguments = ["brew", "services", "start", "postgresql@17"]
        start.environment = env
        start.standardOutput = Pipe()
        start.standardError = Pipe()
        if (try? start.run()) != nil {
            start.waitUntilExit()
            Thread.sleep(forTimeInterval: 1.0)
        }
    }

    private func ensureBackendRunning(completion: @escaping (Bool, String?) -> Void) {
        if isHealthy() {
            startedByApp = false
            completion(true, nil)
            return
        }

        let env = enrichedEnvironment()
        ensurePostgresRunning(env: env)

        let binPath = "\(appDir)/artex"
        guard FileManager.default.isExecutableFile(atPath: binPath) else {
            completion(false, "Binário não encontrado em \(binPath). Execute ./build.sh primeiro.")
            return
        }

        let logDir = "\(NSHomeDirectory())/Library/Logs/Artex"
        try? FileManager.default.createDirectory(atPath: logDir, withIntermediateDirectories: true)
        let logPath = "\(logDir)/server.log"
        if !FileManager.default.fileExists(atPath: logPath) {
            FileManager.default.createFile(atPath: logPath, contents: nil)
        }
        let logHandle = FileHandle(forWritingAtPath: logPath)
        logHandle?.seekToEndOfFile()

        let proc = Process()
        proc.executableURL = URL(fileURLWithPath: binPath)
        proc.currentDirectoryURL = URL(fileURLWithPath: appDir)
        proc.arguments = ["-addr", serverAddr, "-proxy", proxyAddr]
        proc.environment = env
        if let fh = logHandle {
            proc.standardOutput = fh
            proc.standardError = fh
        }

        do {
            try proc.run()
        } catch {
            completion(false, error.localizedDescription)
            return
        }

        serverProcess = proc
        startedByApp = true

        let deadline = Date().addingTimeInterval(20.0)
        while Date() < deadline {
            if !proc.isRunning {
                completion(false, "O processo ./artex encerrou com código \(proc.terminationStatus). Veja \(logPath)")
                return
            }
            if isHealthy() {
                completion(true, nil)
                return
            }
            Thread.sleep(forTimeInterval: 0.2)
        }
        completion(false, "Timeout aguardando http://\(serverAddr)/api/health. Veja \(logPath)")
    }

    // MARK: - Menus

    private func buildMenus() {
        let mainMenu = NSMenu()

        // App menu
        let appItem = NSMenuItem()
        mainMenu.addItem(appItem)
        let appMenu = NSMenu()
        appMenu.addItem(withTitle: "Sobre o Artex", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        appMenu.addItem(.separator())
        appMenu.addItem(withTitle: "Ocultar Artex", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        appMenu.addItem(withTitle: "Encerrar Artex", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        appItem.submenu = appMenu

        // Edit menu (essential for Cmd+C/V/X/A/Z in WKWebView)
        let editItem = NSMenuItem()
        mainMenu.addItem(editItem)
        let editMenu = NSMenu(title: "Editar")
        editMenu.addItem(withTitle: "Desfazer", action: Selector(("undo:")), keyEquivalent: "z")
        editMenu.addItem(withTitle: "Refazer", action: Selector(("redo:")), keyEquivalent: "Z")
        editMenu.addItem(.separator())
        editMenu.addItem(withTitle: "Recortar", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        editMenu.addItem(withTitle: "Copiar", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        editMenu.addItem(withTitle: "Colar", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        editMenu.addItem(withTitle: "Selecionar Tudo", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        editItem.submenu = editMenu

        // View menu
        let viewItem = NSMenuItem()
        mainMenu.addItem(viewItem)
        let viewMenu = NSMenu(title: "Visualizar")
        viewMenu.addItem(withTitle: "Recarregar", action: #selector(reloadPage), keyEquivalent: "r")
        viewMenu.addItem(withTitle: "Voltar", action: #selector(goBack), keyEquivalent: "[")
        viewMenu.addItem(withTitle: "Avançar", action: #selector(goForward), keyEquivalent: "]")
        viewMenu.addItem(.separator())
        viewMenu.addItem(withTitle: "Aumentar Zoom", action: #selector(zoomIn), keyEquivalent: "+")
        viewMenu.addItem(withTitle: "Diminuir Zoom", action: #selector(zoomOut), keyEquivalent: "-")
        viewMenu.addItem(withTitle: "Tamanho Real", action: #selector(zoomReset), keyEquivalent: "0")
        viewMenu.addItem(.separator())
        viewMenu.addItem(withTitle: "Abrir no Navegador", action: #selector(openInBrowser), keyEquivalent: "o")
        viewItem.submenu = viewMenu

        // Window menu
        let winItem = NSMenuItem()
        mainMenu.addItem(winItem)
        let winMenu = NSMenu(title: "Janela")
        winMenu.addItem(withTitle: "Minimizar", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        winMenu.addItem(withTitle: "Fechar", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        winItem.submenu = winMenu

        NSApp.mainMenu = mainMenu
    }

    @objc private func reloadPage() { webView.reload() }
    @objc private func goBack() { webView.goBack() }
    @objc private func goForward() { webView.goForward() }
    @objc private func zoomIn() { webView.pageZoom = min(2.0, webView.pageZoom + 0.1) }
    @objc private func zoomOut() { webView.pageZoom = max(0.5, webView.pageZoom - 0.1) }
    @objc private func zoomReset() { webView.pageZoom = 1.0 }
    @objc private func openInBrowser() {
        NSWorkspace.shared.open(webView.url ?? baseURL)
    }

    // MARK: - WKUIDelegate (file uploads, alerts, external links)

    func webView(
        _ webView: WKWebView,
        runOpenPanelWith parameters: WKOpenPanelParameters,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping ([URL]?) -> Void
    ) {
        let panel = NSOpenPanel()
        panel.canChooseFiles = true
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.beginSheetModal(for: window) { resp in
            completionHandler(resp == .OK ? panel.urls : nil)
        }
    }

    func webView(
        _ webView: WKWebView,
        createWebViewWith configuration: WKWebViewConfiguration,
        for navigationAction: WKNavigationAction,
        windowFeatures: WKWindowFeatures
    ) -> WKWebView? {
        if let url = navigationAction.request.url {
            if url.host == "127.0.0.1" || url.host == "localhost" {
                webView.load(navigationAction.request)
            } else {
                NSWorkspace.shared.open(url)
            }
        }
        return nil
    }

    func webView(
        _ webView: WKWebView,
        runJavaScriptAlertPanelWithMessage message: String,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping () -> Void
    ) {
        let alert = NSAlert()
        alert.messageText = "Artex"
        alert.informativeText = message
        alert.addButton(withTitle: "OK")
        alert.beginSheetModal(for: window) { _ in completionHandler() }
    }

    func webView(
        _ webView: WKWebView,
        runJavaScriptConfirmPanelWithMessage message: String,
        initiatedByFrame frame: WKFrameInfo,
        completionHandler: @escaping (Bool) -> Void
    ) {
        let alert = NSAlert()
        alert.messageText = "Artex"
        alert.informativeText = message
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancelar")
        alert.beginSheetModal(for: window) { resp in completionHandler(resp == .alertFirstButtonReturn) }
    }

    // MARK: - WKNavigationDelegate & Downloads

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationAction: WKNavigationAction,
        decisionHandler: @escaping (WKNavigationActionPolicy) -> Void
    ) {
        if navigationAction.shouldPerformDownload {
            decisionHandler(.download)
            return
        }
        if let url = navigationAction.request.url,
           let scheme = url.scheme?.lowercased(),
           (scheme == "http" || scheme == "https"),
           url.host != "127.0.0.1" && url.host != "localhost" {
            NSWorkspace.shared.open(url)
            decisionHandler(.cancel)
            return
        }
        decisionHandler(.allow)
    }

    func webView(
        _ webView: WKWebView,
        decidePolicyFor navigationResponse: WKNavigationResponse,
        decisionHandler: @escaping (WKNavigationResponsePolicy) -> Void
    ) {
        if !navigationResponse.canShowMIMEType {
            decisionHandler(.download)
            return
        }
        decisionHandler(.allow)
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func download(
        _ download: WKDownload,
        decideDestinationUsing response: URLResponse,
        suggestedFilename: String,
        completionHandler: @escaping (URL?) -> Void
    ) {
        let downloads = FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first!
        var dest = downloads.appendingPathComponent(suggestedFilename)
        let base = dest.deletingPathExtension().lastPathComponent
        let ext = dest.pathExtension
        var idx = 1
        while FileManager.default.fileExists(atPath: dest.path) {
            let name = ext.isEmpty ? "\(base)-\(idx)" : "\(base)-\(idx).\(ext)"
            dest = downloads.appendingPathComponent(name)
            idx += 1
        }
        completionHandler(dest)
    }
}

let app = NSApplication.shared
app.setActivationPolicy(.regular)
let delegate = AppDelegate()
app.delegate = delegate
app.run()
