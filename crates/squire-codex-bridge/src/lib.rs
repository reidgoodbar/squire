use std::collections::HashMap;
use std::collections::HashSet;
use std::ffi::CString;
use std::os::raw::c_char;
use std::os::raw::c_int;
use std::os::raw::c_void;
use std::path::Path;
use std::path::PathBuf;
use std::process::Command;
use std::process::Stdio;
use std::sync::Mutex;
use std::sync::OnceLock;

static HOT_LIBRARY: OnceLock<Option<SquireHotLibrary>> = OnceLock::new();
static AUTO_WARMED_REPOS: OnceLock<Mutex<HashSet<PathBuf>>> = OnceLock::new();
const RTLD_NOW: c_int = 2;

#[cfg(any(target_os = "linux", target_os = "android"))]
#[link(name = "dl")]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
}

#[cfg(not(any(target_os = "linux", target_os = "android")))]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
}

#[repr(C)]
struct SquireHotResultFFI {
    handle: *mut c_void,
    stdout_data: *const u8,
    stdout_len: u32,
    stderr_data: *const u8,
    stderr_len: u32,
    exit_code: c_int,
    native_wall_ms: u64,
}

type SquireHotTryReplayCommand = unsafe extern "C" fn(
    cwd: *const c_char,
    argc: c_int,
    argv: *const *const c_char,
    envc: c_int,
    env: *const *const c_char,
    out: *mut SquireHotResultFFI,
) -> c_int;
type SquireHotRecordReplay = unsafe extern "C" fn(result: *mut SquireHotResultFFI);
type SquireHotRelease = unsafe extern "C" fn(result: *mut SquireHotResultFFI);

struct SquireHotLibrary {
    handle: *mut c_void,
    try_replay_command: SquireHotTryReplayCommand,
    record_replay: SquireHotRecordReplay,
    release: SquireHotRelease,
}

unsafe impl Send for SquireHotLibrary {}
unsafe impl Sync for SquireHotLibrary {}

impl Drop for SquireHotLibrary {
    fn drop(&mut self) {
        unsafe {
            dlclose(self.handle);
        }
    }
}

pub struct ReplayOutput {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub exit_code: i32,
}

pub fn try_replay(
    command: &[String],
    cwd: &Path,
    env: &HashMap<String, String>,
) -> Option<ReplayOutput> {
    if !bridge_enabled() {
        trace("disabled");
        return None;
    }
    trace(&format!(
        "called cwd={} argc={} argv={}",
        cwd.display(),
        command.len(),
        shell_join(command)
    ));
    if !command_may_hit(command) {
        trace("skip obvious non-candidate");
        return None;
    }
    let Some(library) = HOT_LIBRARY.get_or_init(load_hot_library).as_ref() else {
        trace("hot library unavailable");
        return None;
    };
    if let Some(output) = try_replay_with_library(library, command, cwd, env) {
        return Some(output);
    }
    if auto_warm_once(cwd) {
        trace("retry after auto warm");
        return try_replay_with_library(library, command, cwd, env);
    }
    None
}

fn try_replay_with_library(
    library: &SquireHotLibrary,
    command: &[String],
    cwd: &Path,
    env: &HashMap<String, String>,
) -> Option<ReplayOutput> {
    let cwd = CString::new(cwd.to_string_lossy().as_bytes()).ok()?;
    let argv_cstrings = command
        .iter()
        .map(|arg| CString::new(arg.as_bytes()))
        .collect::<Result<Vec<_>, _>>()
        .ok()?;
    let argv_ptrs = argv_cstrings
        .iter()
        .map(|arg| arg.as_ptr())
        .collect::<Vec<_>>();
    let env_cstrings = env
        .iter()
        .map(|(key, value)| CString::new(format!("{key}={value}")))
        .collect::<Result<Vec<_>, _>>()
        .ok()?;
    let env_ptrs = env_cstrings
        .iter()
        .map(|entry| entry.as_ptr())
        .collect::<Vec<_>>();
    let mut result = SquireHotResultFFI {
        handle: std::ptr::null_mut(),
        stdout_data: std::ptr::null(),
        stdout_len: 0,
        stderr_data: std::ptr::null(),
        stderr_len: 0,
        exit_code: 0,
        native_wall_ms: 0,
    };
    let hit = unsafe {
        (library.try_replay_command)(
            cwd.as_ptr(),
            argv_ptrs.len() as c_int,
            argv_ptrs.as_ptr(),
            env_ptrs.len() as c_int,
            env_ptrs.as_ptr(),
            &mut result,
        )
    };
    if hit != 1 || result.handle.is_null() {
        trace(&format!(
            "miss code={hit} handle={}",
            !result.handle.is_null()
        ));
        return None;
    }
    let stdout = ffi_bytes(result.stdout_data, result.stdout_len)?;
    let stderr = ffi_bytes(result.stderr_data, result.stderr_len)?;
    let exit_code = result.exit_code;
    unsafe {
        (library.record_replay)(&mut result);
        (library.release)(&mut result);
    }
    trace("direct hot replay hit");
    Some(ReplayOutput {
        stdout,
        stderr,
        exit_code,
    })
}

fn auto_warm_once(cwd: &Path) -> bool {
    if !auto_warm_enabled() {
        return false;
    }
    let Some(git_dir) = discover_git_dir(cwd) else {
        trace("auto warm skipped: no git dir");
        return false;
    };
    let repo_key = git_dir.clone();
    let warmed = AUTO_WARMED_REPOS.get_or_init(|| Mutex::new(HashSet::new()));
    {
        let Ok(mut guard) = warmed.lock() else {
            return false;
        };
        if !guard.insert(repo_key) {
            trace("auto warm skipped: repo already attempted");
            return false;
        }
    }
    let Some(squire) = find_squire_binary() else {
        trace("auto warm skipped: squire binary unavailable");
        return false;
    };
    trace(&format!("auto warm start {}", cwd.display()));
    let status = Command::new(squire)
        .arg("kernel")
        .arg("warm")
        .arg("--short")
        .current_dir(cwd)
        .env("GIT_OPTIONAL_LOCKS", "0")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    match status {
        Ok(status) if status.success() => {
            trace("auto warm ok");
            true
        }
        Ok(status) => {
            trace(&format!("auto warm failed status={status}"));
            false
        }
        Err(err) => {
            trace(&format!("auto warm failed err={err}"));
            false
        }
    }
}

fn auto_warm_enabled() -> bool {
    !matches!(
        std::env::var("SQUIRE_CODEX_AUTO_WARM")
            .ok()
            .map(|value| value.to_ascii_lowercase()),
        Some(value) if matches!(value.as_str(), "0" | "false" | "no" | "off")
    )
}

fn find_squire_binary() -> Option<PathBuf> {
    if let Ok(path) = std::env::var("SQUIRE_CODEX_SQUIRE") {
        if !path.is_empty() {
            let candidate = PathBuf::from(path);
            if candidate.is_file() {
                return Some(candidate);
            }
        }
    }
    find_on_path(if cfg!(target_os = "windows") {
        "squire.exe"
    } else {
        "squire"
    })
}

fn discover_git_dir(cwd: &Path) -> Option<PathBuf> {
    let mut dir = std::fs::canonicalize(cwd)
        .ok()
        .or_else(|| Some(cwd.to_path_buf()))?;
    loop {
        let dot_git = dir.join(".git");
        if dot_git.is_dir() {
            return Some(dot_git);
        }
        if dot_git.is_file() {
            if let Some(path) = parse_gitdir_file(&dot_git, &dir) {
                return Some(path);
            }
        }
        if !dir.pop() {
            return None;
        }
    }
}

fn parse_gitdir_file(path: &Path, worktree_dir: &Path) -> Option<PathBuf> {
    let content = std::fs::read_to_string(path).ok()?;
    let raw = content.strip_prefix("gitdir:")?.trim();
    if raw.is_empty() {
        return None;
    }
    let candidate = PathBuf::from(raw);
    let path = if candidate.is_absolute() {
        candidate
    } else {
        worktree_dir.join(candidate)
    };
    Some(std::fs::canonicalize(&path).unwrap_or(path))
}

fn shell_join(command: &[String]) -> String {
    command
        .iter()
        .map(|arg| {
            if arg
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'/' | b'.' | b'_' | b'-'))
            {
                arg.clone()
            } else {
                format!("{arg:?}")
            }
        })
        .collect::<Vec<_>>()
        .join(" ")
}

fn command_may_hit(command: &[String]) -> bool {
    let Some(program) = command.first() else {
        return false;
    };
    if is_shell_name(program) {
        if let Some(script) = shell_script_arg(command) {
            return shell_script_may_hit(script);
        }
    }
    direct_command_may_hit(command)
}

fn is_shell_name(program: &str) -> bool {
    matches!(base_name_str(program), "sh" | "bash" | "zsh")
}

fn shell_script_arg(command: &[String]) -> Option<&str> {
    let flag_index = command
        .iter()
        .position(|arg| matches!(arg.as_str(), "-c" | "-lc"))?;
    command.get(flag_index + 1).map(String::as_str)
}

fn shell_script_may_hit(script: &str) -> bool {
    let mut token = String::new();
    for byte in script.bytes() {
        if is_shell_word_byte(byte) {
            token.push(byte as char);
            if token.len() > 128 {
                token.clear();
            }
            continue;
        }
        if !token.is_empty() {
            if shell_token_may_hit(&token) {
                return true;
            }
            token.clear();
        }
    }
    !token.is_empty() && shell_token_may_hit(&token)
}

fn shell_token_may_hit(token: &str) -> bool {
    if matches!(
        token,
        "if" | "then"
            | "else"
            | "elif"
            | "fi"
            | "for"
            | "while"
            | "until"
            | "do"
            | "done"
            | "case"
            | "esac"
            | "in"
            | "true"
            | "false"
    ) {
        return false;
    }
    tool_may_hit(token)
}

fn direct_command_may_hit(command: &[String]) -> bool {
    let Some(program) = command.first() else {
        return false;
    };
    let tool = base_name_str(program);
    if matches!(
        tool,
        "git"
            | "cat"
            | "sed"
            | "head"
            | "tail"
            | "file"
            | "grep"
            | "rg"
            | "ls"
            | "which"
            | "command"
            | "printenv"
            | "whoami"
            | "uname"
            | "id"
            | "hostname"
    ) {
        return true;
    }
    is_version_probe(command)
}

fn is_version_probe(command: &[String]) -> bool {
    let Some(program) = command.first() else {
        return false;
    };
    if !matches!(
        base_name_str(program),
        "pip"
            | "pip3"
            | "python"
            | "python3"
            | "node"
            | "npm"
            | "pnpm"
            | "yarn"
            | "go"
            | "cargo"
            | "rustc"
            | "make"
    ) {
        return false;
    }
    command.len() == 2 && matches!(command[1].as_str(), "--version" | "version")
}

fn is_shell_word_byte(byte: u8) -> bool {
    byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'.' | b'_' | b'-')
}

fn tool_may_hit(tool: &str) -> bool {
    matches!(
        base_name_str(tool),
        "git"
            | "cat"
            | "sed"
            | "head"
            | "tail"
            | "file"
            | "grep"
            | "rg"
            | "ls"
            | "which"
            | "command"
            | "printenv"
            | "whoami"
            | "uname"
            | "id"
            | "hostname"
            | "pip"
            | "pip3"
            | "python"
            | "python3"
            | "node"
            | "npm"
            | "pnpm"
            | "yarn"
            | "go"
            | "cargo"
            | "rustc"
            | "make"
    )
}

fn base_name_str(path: &str) -> &str {
    path.rsplit('/').next().unwrap_or(path)
}

fn ffi_bytes(ptr: *const u8, len: u32) -> Option<Vec<u8>> {
    if len == 0 {
        return Some(Vec::new());
    }
    if ptr.is_null() {
        return None;
    }
    Some(unsafe { std::slice::from_raw_parts(ptr, len as usize).to_vec() })
}

fn load_hot_library() -> Option<SquireHotLibrary> {
    for candidate in hot_library_candidates() {
        if let Some(library) = unsafe { load_hot_library_at(&candidate) } {
            trace(&format!(
                "direct hot library loaded {}",
                candidate.display()
            ));
            return Some(library);
        }
    }
    None
}

fn hot_library_candidates() -> Vec<PathBuf> {
    let mut out = Vec::new();
    for key in ["SQUIRE_CODEX_HOT_LIB", "SQUIRE_HOT_LIB"] {
        if let Ok(path) = std::env::var(key) {
            if !path.is_empty() {
                push_candidate(&mut out, PathBuf::from(path));
            }
        }
    }
    if let Ok(squire) = std::env::var("SQUIRE_CODEX_SQUIRE") {
        let path = PathBuf::from(squire);
        if let Some(parent) = path.parent() {
            push_candidate(&mut out, parent.join(hot_library_name()));
            push_candidate(&mut out, parent.join("lib").join(hot_library_name()));
        }
    }
    if let Ok(exe) = std::env::current_exe() {
        if let Some(parent) = exe.parent() {
            push_candidate(&mut out, parent.join(hot_library_name()));
            push_candidate(&mut out, parent.join("lib").join(hot_library_name()));
        }
    }
    if let Some(squire) = find_on_path(if cfg!(target_os = "windows") {
        "squire.exe"
    } else {
        "squire"
    }) {
        if let Some(parent) = squire.parent() {
            push_candidate(&mut out, parent.join(hot_library_name()));
            push_candidate(&mut out, parent.join("lib").join(hot_library_name()));
        }
    }
    out
}

fn hot_library_name() -> &'static str {
    if cfg!(target_os = "macos") {
        "libsquire_hot.dylib"
    } else if cfg!(target_os = "windows") {
        "squire_hot.dll"
    } else {
        "libsquire_hot.so"
    }
}

fn push_candidate(out: &mut Vec<PathBuf>, path: PathBuf) {
    if !out.contains(&path) {
        out.push(path);
    }
}

fn find_on_path(binary: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|dir| dir.join(binary))
        .find(|candidate| candidate.is_file())
}

unsafe fn load_hot_library_at(path: &PathBuf) -> Option<SquireHotLibrary> {
    let path = CString::new(path.to_string_lossy().as_bytes()).ok()?;
    let handle = unsafe { dlopen(path.as_ptr(), RTLD_NOW) };
    if handle.is_null() {
        return None;
    }
    unsafe fn symbol<T: Copy>(handle: *mut c_void, name: &[u8]) -> Option<T> {
        let ptr = unsafe { dlsym(handle, name.as_ptr().cast()) };
        if ptr.is_null() {
            return None;
        }
        Some(unsafe { std::mem::transmute_copy(&ptr) })
    }
    let try_replay_command = unsafe { symbol(handle, b"squire_hot_try_replay_command\0") };
    let record_replay = unsafe { symbol(handle, b"squire_hot_record_replay\0") };
    let release = unsafe { symbol(handle, b"squire_hot_release\0") };
    let (Some(try_replay_command), Some(record_replay), Some(release)) =
        (try_replay_command, record_replay, release)
    else {
        unsafe {
            dlclose(handle);
        }
        return None;
    };
    Some(SquireHotLibrary {
        handle,
        try_replay_command,
        record_replay,
        release,
    })
}

fn bridge_enabled() -> bool {
    !matches!(
        std::env::var("SQUIRE_CODEX_BRIDGE")
            .ok()
            .map(|value| value.to_ascii_lowercase()),
        Some(value) if matches!(value.as_str(), "0" | "false" | "no" | "off")
    )
}

fn trace(message: &str) {
    if matches!(
        std::env::var("SQUIRE_CODEX_BRIDGE_TRACE").ok().as_deref(),
        Some("1") | Some("true") | Some("yes")
    ) {
        eprintln!("squire-codex bridge: {message}");
        let path = std::env::var("SQUIRE_CODEX_BRIDGE_TRACE_FILE")
            .unwrap_or_else(|_| "/tmp/squire-codex-bridge-trace.log".to_string());
        if let Ok(mut file) = std::fs::OpenOptions::new()
            .create(true)
            .append(true)
            .open(path)
        {
            use std::io::Write;
            let _ = writeln!(file, "squire-codex bridge: {message}");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn argv(args: &[&str]) -> Vec<String> {
        args.iter().map(|arg| (*arg).to_string()).collect()
    }

    unsafe extern "C" fn fake_try_replay_command(
        _cwd: *const c_char,
        argc: c_int,
        _argv: *const *const c_char,
        _envc: c_int,
        _env: *const *const c_char,
        out: *mut SquireHotResultFFI,
    ) -> c_int {
        assert_eq!(argc, 3);
        static STDOUT: &[u8] = b"";
        static STDERR: &[u8] = b"";
        unsafe {
            *out = SquireHotResultFFI {
                handle: std::ptr::dangling_mut::<c_void>(),
                stdout_data: STDOUT.as_ptr(),
                stdout_len: STDOUT.len() as u32,
                stderr_data: STDERR.as_ptr(),
                stderr_len: STDERR.len() as u32,
                exit_code: 17,
                native_wall_ms: 0,
            };
        }
        1
    }

    unsafe extern "C" fn fake_record_replay(_result: *mut SquireHotResultFFI) {}

    unsafe extern "C" fn fake_release(result: *mut SquireHotResultFFI) {
        unsafe {
            (*result).exit_code = 0;
            (*result).stdout_data = std::ptr::null();
            (*result).stderr_data = std::ptr::null();
            (*result).handle = std::ptr::null_mut();
        }
    }

    #[test]
    fn replay_copies_exit_code_before_release() {
        let library = SquireHotLibrary {
            handle: std::ptr::null_mut(),
            try_replay_command: fake_try_replay_command,
            record_replay: fake_record_replay,
            release: fake_release,
        };
        let command = argv(&["sh", "-c", "grep -F missing file.txt"]);
        let env = HashMap::new();

        let output = try_replay_with_library(&library, &command, Path::new("."), &env)
            .expect("fake replay should hit");

        assert_eq!(output.exit_code, 17);
        std::mem::forget(library);
    }

    #[test]
    fn command_gate_allows_known_hit_shapes() {
        assert!(command_may_hit(&argv(&["git", "rev-parse", "HEAD"])));
        assert!(command_may_hit(&argv(&[
            "sh",
            "-c",
            "git branch --show-current && git rev-parse HEAD",
        ])));
        assert!(command_may_hit(&argv(&[
            "sh",
            "-c",
            "git ls-files src/flask | wc -l",
        ])));
        assert!(command_may_hit(&argv(&[
            "sh",
            "-c",
            "sed -n '1,80p' src/flask/app.py | tail -n 20",
        ])));
        assert!(command_may_hit(&argv(&[
            "/bin/zsh",
            "-lc",
            "rg -F token src/flask/app.py | head -n 1",
        ])));
    }

    #[test]
    fn command_gate_skips_obvious_non_candidates() {
        assert!(!command_may_hit(&argv(&["nl", "-ba", "src/flask/app.py"])));
        assert!(!command_may_hit(&argv(&[
            "sh",
            "-c",
            "nl -ba src/flask/app.py",
        ])));
        assert!(!command_may_hit(&argv(
            &["sh", "-c", "echo hello | wc -c",]
        )));
        assert!(!command_may_hit(&argv(&[
            "python3",
            "-c",
            "print('fallback')",
        ])));
    }
}
