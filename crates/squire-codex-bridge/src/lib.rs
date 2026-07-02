use std::collections::HashMap;
use std::ffi::CString;
use std::os::raw::c_char;
use std::os::raw::c_int;
use std::os::raw::c_void;
use std::path::Path;
use std::path::PathBuf;
use std::sync::OnceLock;

static HOT_LIBRARY: OnceLock<Option<SquireHotLibrary>> = OnceLock::new();
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
        return None;
    }
    let library = HOT_LIBRARY.get_or_init(load_hot_library).as_ref()?;
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
        return None;
    }
    let stdout = ffi_bytes(result.stdout_data, result.stdout_len)?;
    let stderr = ffi_bytes(result.stderr_data, result.stderr_len)?;
    unsafe {
        (library.record_replay)(&mut result);
        (library.release)(&mut result);
    }
    trace("direct hot replay hit");
    Some(ReplayOutput {
        stdout,
        stderr,
        exit_code: result.exit_code,
    })
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
                out.push(PathBuf::from(path));
            }
        }
    }
    if let Ok(squire) = std::env::var("SQUIRE_CODEX_SQUIRE") {
        let lib_name = if cfg!(target_os = "macos") {
            "libsquire_hot.dylib"
        } else {
            "libsquire_hot.so"
        };
        let path = PathBuf::from(squire);
        if let Some(parent) = path.parent() {
            out.push(parent.join(lib_name));
            out.push(parent.join("lib").join(lib_name));
        }
    }
    out
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
    matches!(
        std::env::var("SQUIRE_CODEX_BRIDGE").ok().as_deref(),
        Some("1") | Some("true") | Some("yes")
    )
}

fn trace(message: &str) {
    if matches!(
        std::env::var("SQUIRE_CODEX_BRIDGE_TRACE").ok().as_deref(),
        Some("1") | Some("true") | Some("yes")
    ) {
        eprintln!("squire-codex bridge: {message}");
    }
}
