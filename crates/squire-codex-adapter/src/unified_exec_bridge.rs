use std::collections::HashMap;
use std::sync::Arc;

use codex_protocol::protocol::ExecCommandSource;
use codex_utils_output_truncation::approx_token_count;
use codex_utils_path_uri::PathUri;
use tokio::time::Instant;

use crate::exec::squire_bridge as exec_squire_bridge;
use crate::exec_env::CODEX_THREAD_ID_ENV_VAR;
use crate::exec_env::create_env;
use crate::exec_env::inject_permission_profile_env;
use crate::tools::context::ExecCommandToolOutput;
use crate::tools::events::ToolEmitter;
use crate::tools::events::ToolEventCtx;
use crate::tools::events::ToolEventStage;
use crate::unified_exec::ExecCommandRequest;
use crate::unified_exec::UnifiedExecContext;
use crate::unified_exec::UnifiedExecError;
use crate::unified_exec::UnifiedExecProcessManager;
use crate::unified_exec::async_watcher::emit_exec_end_for_unified_exec;
use crate::unified_exec::generate_chunk_id;
use crate::unified_exec::head_tail_buffer::HeadTailBuffer;

const UNIFIED_EXEC_ENV: [(&str, &str); 10] = [
    ("NO_COLOR", "1"),
    ("TERM", "dumb"),
    ("LANG", "C.UTF-8"),
    ("LC_CTYPE", "C.UTF-8"),
    ("LC_ALL", "C.UTF-8"),
    ("COLORTERM", ""),
    ("PAGER", "cat"),
    ("GIT_PAGER", "cat"),
    ("GH_PAGER", "cat"),
    ("CODEX_CI", "1"),
];

pub(super) async fn try_exec_command(
    manager: &UnifiedExecProcessManager,
    request: &ExecCommandRequest,
    cwd: PathUri,
    context: &UnifiedExecContext,
) -> Result<Option<ExecCommandToolOutput>, UnifiedExecError> {
    let native_cwd = cwd
        .to_abs_path()
        .map_err(|_| UnifiedExecError::ForeignPath { path: cwd.clone() })?;
    let local_policy_env = create_env(
        &context.turn.config.permissions.shell_environment_policy,
        /*thread_id*/ None,
    );
    let mut env = local_policy_env;
    env.insert(
        CODEX_THREAD_ID_ENV_VAR.to_string(),
        context.session.thread_id.to_string(),
    );
    let active_permission_profile = context.turn.config.permissions.active_permission_profile();
    inject_permission_profile_env(&mut env, active_permission_profile.as_ref());
    let env = apply_unified_exec_env(env);

    let start = Instant::now();
    let Some(replay) =
        exec_squire_bridge::try_replay_bytes(&request.command, &native_cwd, &env).await
    else {
        return Ok(None);
    };
    let wall_time = Instant::now().saturating_duration_since(start);

    let mut raw_output = replay.stdout;
    raw_output.extend_from_slice(&replay.stderr);
    let text = String::from_utf8_lossy(&raw_output).to_string();
    let chunk_id = generate_chunk_id();
    let original_token_count = approx_token_count(&text);
    let transcript = Arc::new(tokio::sync::Mutex::new(HeadTailBuffer::default()));
    if !raw_output.is_empty() {
        let mut guard = transcript.lock().await;
        guard.push_chunk(raw_output.clone());
    }

    let event_ctx = ToolEventCtx::new(
        context.session.as_ref(),
        context.turn.as_ref(),
        &context.call_id,
        /*turn_diff_tracker*/ None,
    );
    let emitter = ToolEmitter::unified_exec(
        &request.command,
        cwd.clone(),
        ExecCommandSource::UnifiedExecStartup,
        Some(request.process_id.to_string()),
    );
    emitter.emit(event_ctx, ToolEventStage::Begin).await;
    emit_exec_end_for_unified_exec(
        Arc::clone(&context.session),
        Arc::clone(&context.turn),
        context.call_id.clone(),
        request.command.clone(),
        cwd,
        Some(request.process_id.to_string()),
        transcript,
        text,
        replay.exit_code,
        wall_time,
    )
    .await;
    manager.release_process_id(request.process_id).await;

    Ok(Some(ExecCommandToolOutput {
        event_call_id: context.call_id.clone(),
        chunk_id,
        wall_time,
        raw_output,
        truncation_policy: context.turn.model_info.truncation_policy.into(),
        max_output_tokens: request.max_output_tokens,
        process_id: None,
        exit_code: Some(replay.exit_code),
        original_token_count: Some(original_token_count),
        hook_command: Some(request.hook_command.clone()),
    }))
}

fn apply_unified_exec_env(mut env: HashMap<String, String>) -> HashMap<String, String> {
    for (key, value) in UNIFIED_EXEC_ENV {
        env.insert(key.to_string(), value.to_string());
    }
    env
}
