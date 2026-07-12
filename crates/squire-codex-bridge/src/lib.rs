use std::collections::HashMap;
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
use std::time::Duration;
use std::time::Instant;

const RUNTIME_ABI_VERSION: u32 = 1;
const RUNTIME_HIT: c_int = 1;
const RUNTIME_MISS: c_int = 0;
#[cfg(unix)]
const RTLD_NOW: c_int = 2;

static RUNTIME_LIBRARY: OnceLock<Option<SquireRuntimeLibrary>> = OnceLock::new();
static PREPARATION_REQUESTS: OnceLock<Mutex<HashMap<PathBuf, Instant>>> = OnceLock::new();

const PREPARATION_RETRY_AFTER: Duration = Duration::from_secs(5);

#[cfg(any(target_os = "linux", target_os = "android"))]
#[link(name = "dl")]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
}

#[cfg(all(unix, not(any(target_os = "linux", target_os = "android"))))]
unsafe extern "C" {
    fn dlopen(filename: *const c_char, flags: c_int) -> *mut c_void;
    fn dlsym(handle: *mut c_void, symbol: *const c_char) -> *mut c_void;
    fn dlclose(handle: *mut c_void) -> c_int;
}

#[repr(C)]
struct SquireRuntimeResultFFI {
    handle: *mut c_void,
    stdout_data: *const u8,
    stdout_len: u32,
    stderr_data: *const u8,
    stderr_len: u32,
    exit_code: c_int,
    native_wall_ms: u64,
}

type SquireRuntimeABIVersion = unsafe extern "C" fn() -> u32;
type SquireRuntimeTryExecute = unsafe extern "C" fn(
    cwd: *const c_char,
    argc: c_int,
    argv: *const *const c_char,
    envc: c_int,
    env: *const *const c_char,
    out: *mut SquireRuntimeResultFFI,
) -> c_int;
type SquireRuntimeRecordHit = unsafe extern "C" fn(result: *mut SquireRuntimeResultFFI);
type SquireRuntimeRelease = unsafe extern "C" fn(result: *mut SquireRuntimeResultFFI);

struct SquireRuntimeLibrary {
    handle: *mut c_void,
    try_execute: SquireRuntimeTryExecute,
    record_hit: SquireRuntimeRecordHit,
    release: SquireRuntimeRelease,
}

unsafe impl Send for SquireRuntimeLibrary {}
unsafe impl Sync for SquireRuntimeLibrary {}

impl Drop for SquireRuntimeLibrary {
    fn drop(&mut self) {
        #[cfg(unix)]
        if !self.handle.is_null() {
            unsafe {
                dlclose(self.handle);
            }
        }
    }
}

pub struct ReplayOutput {
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub exit_code: i32,
}

/// Attempts a behavior-preserving Squire execution. None always means that
/// Codex must continue through its original native execution path.
pub fn try_replay(
    command: &[String],
    cwd: &Path,
    env: &HashMap<String, String>,
) -> Option<ReplayOutput> {
    if !bridge_enabled() || command.is_empty() {
        return None;
    }
    let Some(library) = RUNTIME_LIBRARY.get_or_init(load_runtime_library).as_ref() else {
        trace("runtime unavailable");
        return None;
    };
    let (decision, output) = try_execute_with_library(library, command, cwd, env);
    if decision == RUNTIME_MISS {
        request_preparation(cwd);
    }
    output
}

fn try_execute_with_library(
    library: &SquireRuntimeLibrary,
    command: &[String],
    cwd: &Path,
    env: &HashMap<String, String>,
) -> (c_int, Option<ReplayOutput>) {
    let Some(cwd) = CString::new(cwd.to_string_lossy().as_bytes()).ok() else {
        return (RUNTIME_MISS, None);
    };
    let Some(argv) = cstring_list(command.iter().map(String::as_str)) else {
        return (RUNTIME_MISS, None);
    };
    let env_values = env
        .iter()
        .map(|(key, value)| format!("{key}={value}"))
        .collect::<Vec<_>>();
    let Some(env) = cstring_list(env_values.iter().map(String::as_str)) else {
        return (RUNTIME_MISS, None);
    };
    let argv_ptrs = argv.iter().map(|value| value.as_ptr()).collect::<Vec<_>>();
    let env_ptrs = env.iter().map(|value| value.as_ptr()).collect::<Vec<_>>();
    let mut result = SquireRuntimeResultFFI {
        handle: std::ptr::null_mut(),
        stdout_data: std::ptr::null(),
        stdout_len: 0,
        stderr_data: std::ptr::null(),
        stderr_len: 0,
        exit_code: 0,
        native_wall_ms: 0,
    };
    let decision = unsafe {
        (library.try_execute)(
            cwd.as_ptr(),
            argv_ptrs.len() as c_int,
            argv_ptrs.as_ptr(),
            env_ptrs.len() as c_int,
            env_ptrs.as_ptr(),
            &mut result,
        )
    };
    if decision != RUNTIME_HIT || result.handle.is_null() {
        return (decision, None);
    }
    let Some(stdout) = ffi_bytes(result.stdout_data, result.stdout_len) else {
        unsafe { (library.release)(&mut result) };
        return (RUNTIME_MISS, None);
    };
    let Some(stderr) = ffi_bytes(result.stderr_data, result.stderr_len) else {
        unsafe { (library.release)(&mut result) };
        return (RUNTIME_MISS, None);
    };
    let exit_code = result.exit_code;
    unsafe {
        (library.record_hit)(&mut result);
        (library.release)(&mut result);
    }
    trace("hit");
    (
        decision,
        Some(ReplayOutput {
            stdout,
            stderr,
            exit_code,
        }),
    )
}

fn cstring_list<'a>(values: impl Iterator<Item = &'a str>) -> Option<Vec<CString>> {
    values
        .map(|value| CString::new(value.as_bytes()))
        .collect::<Result<Vec<_>, _>>()
        .ok()
}

fn request_preparation(cwd: &Path) {
    if !auto_prepare_enabled() {
        return;
    }
    let Some(workspace) = discover_workspace(cwd) else {
        return;
    };
    let Some(squire) = find_squire_binary() else {
        return;
    };
    let requests = PREPARATION_REQUESTS.get_or_init(|| Mutex::new(HashMap::new()));
    let Ok(mut requests) = requests.lock() else {
        return;
    };
    if !mark_preparation_request(&mut requests, workspace.clone(), Instant::now()) {
        return;
    }
    drop(requests);
    let child = Command::new(squire)
        .arg("prepare")
        .arg("--short")
        .current_dir(&workspace)
        .env("GIT_OPTIONAL_LOCKS", "0")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn();
    if let Ok(mut child) = child {
        std::thread::spawn(move || {
            let _ = child.wait();
        });
        trace("preparation requested");
    } else if let Ok(mut requests) = PREPARATION_REQUESTS
        .get_or_init(|| Mutex::new(HashMap::new()))
        .lock()
    {
        requests.remove(&workspace);
    }
}

fn mark_preparation_request(
    requests: &mut HashMap<PathBuf, Instant>,
    workspace: PathBuf,
    now: Instant,
) -> bool {
    if requests
        .get(&workspace)
        .is_some_and(|last| now.saturating_duration_since(*last) < PREPARATION_RETRY_AFTER)
    {
        return false;
    }
    requests.insert(workspace, now);
    true
}

fn discover_workspace(cwd: &Path) -> Option<PathBuf> {
    let mut directory = std::fs::canonicalize(cwd)
        .ok()
        .unwrap_or_else(|| cwd.to_path_buf());
    loop {
        if directory.join(".git").exists() {
            return Some(directory);
        }
        if !directory.pop() {
            return None;
        }
    }
}

fn auto_prepare_enabled() -> bool {
    !is_false_env("SQUIRE_AUTO_PREPARE") && !is_false_env("SQUIRE_CODEX_AUTO_WARM")
}

fn load_runtime_library() -> Option<SquireRuntimeLibrary> {
    for candidate in runtime_library_candidates() {
        if let Some(library) = unsafe { load_runtime_library_at(&candidate) } {
            trace(&format!("runtime loaded {}", candidate.display()));
            return Some(library);
        }
    }
    None
}

fn runtime_library_candidates() -> Vec<PathBuf> {
    let mut candidates = Vec::new();
    for key in [
        "SQUIRE_CODEX_RUNTIME_LIB",
        "SQUIRE_RUNTIME_LIB",
        "SQUIRE_CODEX_HOT_LIB",
        "SQUIRE_HOT_LIB",
    ] {
        match std::env::var(key) {
            Ok(path) if !path.is_empty() => {
                push_candidate(&mut candidates, PathBuf::from(path));
            }
            _ => {}
        }
    }
    if let Some(parent) = std::env::current_exe()
        .ok()
        .and_then(|exe| exe.parent().map(Path::to_path_buf))
    {
        push_runtime_candidates(&mut candidates, &parent);
    }
    if let Some(parent) =
        find_squire_binary().and_then(|squire| squire.parent().map(Path::to_path_buf))
    {
        push_runtime_candidates(&mut candidates, &parent);
    }
    candidates
}

fn push_runtime_candidates(candidates: &mut Vec<PathBuf>, directory: &Path) {
    for name in runtime_library_names() {
        push_candidate(candidates, directory.join(name));
        push_candidate(candidates, directory.join("lib").join(name));
    }
}

fn runtime_library_names() -> [&'static str; 2] {
    if cfg!(target_os = "macos") {
        ["libsquire_runtime.dylib", "libsquire_hot.dylib"]
    } else if cfg!(target_os = "windows") {
        ["squire_runtime.dll", "squire_hot.dll"]
    } else {
        ["libsquire_runtime.so", "libsquire_hot.so"]
    }
}

fn push_candidate(candidates: &mut Vec<PathBuf>, path: PathBuf) {
    if !candidates.contains(&path) {
        candidates.push(path);
    }
}

fn find_squire_binary() -> Option<PathBuf> {
    match std::env::var("SQUIRE_CODEX_SQUIRE") {
        Ok(path) if !path.is_empty() => {
            let path = PathBuf::from(path);
            if path.is_file() {
                return Some(path);
            }
        }
        _ => {}
    }
    find_on_path(if cfg!(target_os = "windows") {
        "squire.exe"
    } else {
        "squire"
    })
}

fn find_on_path(binary: &str) -> Option<PathBuf> {
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path)
        .map(|directory| directory.join(binary))
        .find(|candidate| candidate.is_file())
}

#[cfg(unix)]
unsafe fn load_runtime_library_at(path: &Path) -> Option<SquireRuntimeLibrary> {
    let path = CString::new(path.to_string_lossy().as_bytes()).ok()?;
    let handle = unsafe { dlopen(path.as_ptr(), RTLD_NOW) };
    if handle.is_null() {
        return None;
    }
    unsafe fn symbol<T: Copy>(handle: *mut c_void, name: &[u8]) -> Option<T> {
        let pointer = unsafe { dlsym(handle, name.as_ptr().cast()) };
        if pointer.is_null() {
            return None;
        }
        Some(unsafe { std::mem::transmute_copy(&pointer) })
    }
    let abi_version: Option<SquireRuntimeABIVersion> =
        unsafe { symbol(handle, b"squire_runtime_abi_version\0") };
    let try_execute = unsafe { symbol(handle, b"squire_runtime_try_execute\0") };
    let record_hit = unsafe { symbol(handle, b"squire_runtime_record_hit\0") };
    let release = unsafe { symbol(handle, b"squire_runtime_release\0") };
    let Some(abi_version) = abi_version else {
        unsafe { dlclose(handle) };
        return None;
    };
    if unsafe { abi_version() } != RUNTIME_ABI_VERSION {
        unsafe { dlclose(handle) };
        return None;
    }
    let (Some(try_execute), Some(record_hit), Some(release)) = (try_execute, record_hit, release)
    else {
        unsafe { dlclose(handle) };
        return None;
    };
    Some(SquireRuntimeLibrary {
        handle,
        try_execute,
        record_hit,
        release,
    })
}

#[cfg(not(unix))]
unsafe fn load_runtime_library_at(_path: &Path) -> Option<SquireRuntimeLibrary> {
    None
}

fn ffi_bytes(pointer: *const u8, length: u32) -> Option<Vec<u8>> {
    if length == 0 {
        return Some(Vec::new());
    }
    if pointer.is_null() {
        return None;
    }
    Some(unsafe { std::slice::from_raw_parts(pointer, length as usize).to_vec() })
}

fn bridge_enabled() -> bool {
    !is_false_env("SQUIRE_CODEX_BRIDGE")
}

fn is_false_env(key: &str) -> bool {
    matches!(
        std::env::var(key)
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
        eprintln!("squire runtime: {message}");
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    unsafe extern "C" fn fake_try_execute(
        _cwd: *const c_char,
        _argc: c_int,
        _argv: *const *const c_char,
        _envc: c_int,
        _env: *const *const c_char,
        out: *mut SquireRuntimeResultFFI,
    ) -> c_int {
        static STDOUT: &[u8] = b"output\n";
        unsafe {
            (*out).handle = std::ptr::dangling_mut::<c_void>();
            (*out).stdout_data = STDOUT.as_ptr();
            (*out).stdout_len = STDOUT.len() as u32;
            (*out).exit_code = 7;
        }
        RUNTIME_HIT
    }

    unsafe extern "C" fn fake_record_hit(_result: *mut SquireRuntimeResultFFI) {}

    unsafe extern "C" fn fake_release(result: *mut SquireRuntimeResultFFI) {
        unsafe { *result = std::mem::zeroed() };
    }

    #[test]
    fn result_is_copied_before_release() {
        let library = SquireRuntimeLibrary {
            handle: std::ptr::null_mut(),
            try_execute: fake_try_execute,
            record_hit: fake_record_hit,
            release: fake_release,
        };
        let command = vec![
            "git".to_string(),
            "rev-parse".to_string(),
            "HEAD".to_string(),
        ];
        let (decision, output) =
            try_execute_with_library(&library, &command, Path::new("/tmp"), &HashMap::new());
        let output = output.expect("hit");
        assert_eq!(decision, RUNTIME_HIT);
        assert_eq!(output.stdout, b"output\n");
        assert_eq!(output.stderr, b"");
        assert_eq!(output.exit_code, 7);
    }

    #[test]
    fn discovers_nearest_workspace() {
        let root = std::env::temp_dir().join(format!(
            "squire-runtime-workspace-{}-{}",
            std::process::id(),
            std::thread::current().name().unwrap_or("test")
        ));
        let nested = root.join("a").join("b");
        std::fs::create_dir_all(root.join(".git")).expect("git directory");
        std::fs::create_dir_all(&nested).expect("nested directory");
        let discovered = discover_workspace(&nested).expect("workspace");
        assert_eq!(
            discovered,
            std::fs::canonicalize(&root).expect("canonical root")
        );
        std::fs::remove_dir_all(root).expect("remove fixture");
    }

    #[test]
    fn preparation_requests_are_deduplicated_then_retryable() {
        let workspace = PathBuf::from("/tmp/squire-preparation-workspace");
        let start = Instant::now();
        let mut requests = HashMap::new();
        assert!(mark_preparation_request(
            &mut requests,
            workspace.clone(),
            start
        ));
        assert!(!mark_preparation_request(
            &mut requests,
            workspace.clone(),
            start + Duration::from_secs(1)
        ));
        assert!(mark_preparation_request(
            &mut requests,
            workspace,
            start + PREPARATION_RETRY_AFTER
        ));
    }
}
