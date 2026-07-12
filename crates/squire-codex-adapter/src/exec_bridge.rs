use std::collections::HashMap;
use std::time::Instant;

use codex_protocol::exec_output::StreamOutput;
use codex_protocol::protocol::Event;
use codex_protocol::protocol::EventMsg;
use codex_protocol::protocol::ExecCommandOutputDeltaEvent;
use codex_protocol::protocol::ExecOutputStream;
use codex_utils_absolute_path::AbsolutePathBuf;

use super::ExecCapturePolicy;
use super::RawExecToolCallOutput;
use super::StdoutStream;

pub(crate) struct SquireReplayOutput {
    pub(crate) stdout: Vec<u8>,
    pub(crate) stderr: Vec<u8>,
    pub(crate) exit_code: i32,
}

pub(super) async fn try_exec(
    command: &[String],
    cwd: &AbsolutePathBuf,
    env: &HashMap<String, String>,
    capture_policy: ExecCapturePolicy,
    stdout_stream: Option<StdoutStream>,
) -> Option<RawExecToolCallOutput> {
    let output = try_replay_bytes(command, cwd, env).await?;
    emit_replay_stream(stdout_stream.as_ref(), &output.stdout, false).await;
    emit_replay_stream(stdout_stream.as_ref(), &output.stderr, true).await;
    let stdout = retain_output(output.stdout, capture_policy.retained_bytes_cap());
    let stderr = retain_output(output.stderr, capture_policy.retained_bytes_cap());
    let aggregated_output =
        super::aggregate_output(&stdout, &stderr, capture_policy.retained_bytes_cap());
    Some(RawExecToolCallOutput {
        exit_status: super::synthetic_exit_status_for_code(output.exit_code),
        stdout,
        stderr,
        aggregated_output,
        timed_out: false,
    })
}

pub(crate) async fn try_replay_bytes(
    command: &[String],
    cwd: &AbsolutePathBuf,
    env: &HashMap<String, String>,
) -> Option<SquireReplayOutput> {
    let output = super::squire_codex_bridge::try_replay(command, cwd.as_path(), env)?;
    Some(SquireReplayOutput {
        stdout: output.stdout,
        stderr: output.stderr,
        exit_code: output.exit_code,
    })
}

pub(crate) async fn try_replay_shell_command(
    command: &str,
    cwd: &AbsolutePathBuf,
    env: &HashMap<String, String>,
) -> Option<SquireReplayOutput> {
    let argv = ["sh".to_string(), "-c".to_string(), command.to_string()];
    try_replay_bytes(&argv, cwd, env).await
}

pub(crate) async fn try_replay_shell_output(
    command: &str,
    cwd: &AbsolutePathBuf,
    env: &HashMap<String, String>,
) -> Option<codex_protocol::exec_output::ExecToolCallOutput> {
    let start = Instant::now();
    let replay = try_replay_shell_command(command, cwd, env).await?;
    let stdout = String::from_utf8_lossy(&replay.stdout).to_string();
    let stderr = String::from_utf8_lossy(&replay.stderr).to_string();
    let aggregated_output = format!("{stdout}{stderr}");
    Some(codex_protocol::exec_output::ExecToolCallOutput {
        exit_code: replay.exit_code,
        stdout: StreamOutput::new(stdout),
        stderr: StreamOutput::new(stderr),
        aggregated_output: StreamOutput::new(aggregated_output),
        duration: start.elapsed(),
        timed_out: false,
    })
}

fn retain_output(bytes: Vec<u8>, max_bytes: Option<usize>) -> StreamOutput<Vec<u8>> {
    let text = match max_bytes {
        Some(max_bytes) if bytes.len() > max_bytes => bytes[..max_bytes].to_vec(),
        _ => bytes,
    };
    StreamOutput {
        text,
        truncated_after_lines: None,
    }
}

async fn emit_replay_stream(stream: Option<&StdoutStream>, bytes: &[u8], is_stderr: bool) {
    if bytes.is_empty() {
        return;
    }
    let Some(stream) = stream else {
        return;
    };
    let msg = EventMsg::ExecCommandOutputDelta(ExecCommandOutputDeltaEvent {
        call_id: stream.call_id.clone(),
        stream: if is_stderr {
            ExecOutputStream::Stderr
        } else {
            ExecOutputStream::Stdout
        },
        chunk: bytes.to_vec(),
    });
    let event = Event {
        id: stream.sub_id.clone(),
        msg,
    };
    let _ = stream.tx_event.send(event).await;
}
