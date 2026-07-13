#define SQUIRE_MMAP_EMBEDDED
#define SQUIRE_MMAP_HOT_API 1
#define SQUIRE_MMAP_NO_MAIN 1
#define SQUIRE_PRELOAD_HELPER_NO_MAIN

#include "squire_hot_api.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "squire_preload_helper.c"

extern char **environ;

typedef enum {
	SQUIRE_HOT_HANDLE_EXACT = 1,
	SQUIRE_HOT_HANDLE_SCRIPT = 2,
} squire_hot_handle_kind;

typedef struct {
	squire_hot_handle_kind kind;
	prepared_exact_replay exact;
	helper_result script;
} squire_hot_handle;

static void squire_hot_trace(const char *message) {
	const char *enabled = getenv("SQUIRE_CODEX_BRIDGE_TRACE");
	if (enabled != NULL && (strcmp(enabled, "1") == 0 || strcmp(enabled, "true") == 0 || strcmp(enabled, "yes") == 0)) {
		fprintf(stderr, "squire hot api: %s\n", message);
		const char *path = getenv("SQUIRE_CODEX_BRIDGE_TRACE_FILE");
		if (path == NULL || path[0] == '\0') {
			path = "/tmp/squire-codex-bridge-trace.log";
		}
		FILE *f = fopen(path, "a");
		if (f != NULL) {
			fprintf(f, "squire hot api: %s\n", message);
			fclose(f);
		}
	}
}

static void squire_hot_fill_exact(squire_hot_result *out, squire_hot_handle *handle) {
	out->handle = handle;
	out->stdout_data = handle->exact.stdout_data;
	out->stdout_len = handle->exact.stdout_len;
	out->stderr_data = handle->exact.stderr_data;
	out->stderr_len = handle->exact.stderr_len;
	out->exit_code = handle->exact.exit_code;
	out->native_wall_ms = handle->exact.native_wall_ms;
}

static void squire_hot_fill_script(squire_hot_result *out, squire_hot_handle *handle) {
	out->handle = handle;
	out->stdout_data = handle->script.stdout_buf.data;
	out->stdout_len = handle->script.stdout_buf.len;
	out->stderr_data = handle->script.stderr_buf.data;
	out->stderr_len = handle->script.stderr_buf.len;
	out->exit_code = handle->script.exit_code;
	out->native_wall_ms = 0;
}

int squire_hot_try_replay_argv(const char *cwd, int argc, const char *const *argv, squire_hot_result *out) {
	if (out == NULL || argc <= 0 || argc > MAX_ARGC || argv == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	char *mutable_argv[MAX_ARGC];
	for (int i = 0; i < argc; i++) {
		if (argv[i] == NULL) {
			return 0;
		}
		mutable_argv[i] = (char *)argv[i];
	}
	squire_hot_handle *handle = (squire_hot_handle *)calloc(1, sizeof(*handle));
	if (handle == NULL) {
		return 0;
	}
	handle->kind = SQUIRE_HOT_HANDLE_EXACT;
	if (!prepare_exact_replay_at_cwd(cwd, argc, mutable_argv, &handle->exact)) {
		handle->kind = SQUIRE_HOT_HANDLE_SCRIPT;
		if (!helper_prepare_warm_file_at_cwd(cwd, argc, mutable_argv, &handle->script, now_monotonic_ns())) {
			free(handle);
			return 0;
		}
		squire_hot_fill_script(out, handle);
		return 1;
	}
	squire_hot_fill_exact(out, handle);
	return 1;
}

int squire_hot_try_replay_script(const char *cwd, const char *script, squire_hot_result *out) {
	if (out == NULL || script == NULL || script[0] == '\0') {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	squire_hot_handle *handle = (squire_hot_handle *)calloc(1, sizeof(*handle));
	if (handle == NULL) {
		return 0;
	}
	handle->kind = SQUIRE_HOT_HANDLE_SCRIPT;
	if (!helper_eval_shell_ir_at_cwd(cwd, script, &handle->script, now_monotonic_ns())) {
		free(handle);
		return 0;
	}
	squire_hot_fill_script(out, handle);
	return 1;
}

static int squire_hot_key_eq(const char *entry, size_t key_len, const char *key) {
	return strlen(key) == key_len && strncmp(entry, key, key_len) == 0;
}

static int squire_hot_key_has_prefix(const char *entry, size_t key_len, const char *prefix) {
	size_t prefix_len = strlen(prefix);
	return key_len >= prefix_len && strncmp(entry, prefix, prefix_len) == 0;
}

static int squire_hot_sensitive_env_key(const char *entry, size_t key_len) {
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "LANG") ||
	       squire_hot_key_has_prefix(entry, key_len, "LC_") ||
	       squire_hot_key_eq(entry, key_len, "TZ") ||
	       squire_hot_key_eq(entry, key_len, "TERM") ||
	       squire_hot_key_eq(entry, key_len, "COLUMNS") ||
	       squire_hot_key_eq(entry, key_len, "PAGER") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       (key_len >= 4 && strncmp(entry, "GIT_", 4) == 0);
}

static int squire_hot_git_metadata_env_key(const char *entry, size_t key_len) {
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "XDG_CONFIG_HOME") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_REAL_GIT") ||
	       squire_hot_key_eq(entry, key_len, "GIT_DIR") ||
	       squire_hot_key_eq(entry, key_len, "GIT_WORK_TREE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_COMMON_DIR") ||
	       squire_hot_key_eq(entry, key_len, "GIT_NAMESPACE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_INDEX_FILE") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_GLOBAL") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_SYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_NOSYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_COUNT") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CONFIG_PARAMETERS") ||
	       squire_hot_key_has_prefix(entry, key_len, "GIT_CONFIG_KEY_") ||
	       squire_hot_key_has_prefix(entry, key_len, "GIT_CONFIG_VALUE_") ||
	       squire_hot_key_eq(entry, key_len, "GIT_CEILING_DIRECTORIES") ||
	       squire_hot_key_eq(entry, key_len, "GIT_DISCOVERY_ACROSS_FILESYSTEM") ||
	       squire_hot_key_eq(entry, key_len, "GIT_OBJECT_DIRECTORY") ||
	       squire_hot_key_eq(entry, key_len, "GIT_ALTERNATE_OBJECT_DIRECTORIES");
}

static int squire_hot_shell_script_env_key(const char *entry, size_t key_len) {
	if (squire_hot_key_eq(entry, key_len, "GIT_PAGER") ||
	    squire_hot_key_eq(entry, key_len, "GH_PAGER")) {
		return 0;
	}
	return squire_hot_key_eq(entry, key_len, "PATH") ||
	       squire_hot_key_eq(entry, key_len, "HOME") ||
	       squire_hot_key_eq(entry, key_len, "XDG_CONFIG_HOME") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_KERNEL_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_STORE_ROOT") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_SHIM_REAL_PATH") ||
	       squire_hot_key_eq(entry, key_len, "SQUIRE_REAL_GIT") ||
	       (key_len >= 4 && strncmp(entry, "GIT_", 4) == 0);
}

static int squire_hot_shell_script_requires_full_env(const char *script) {
	helper_plan plan;
	if (script == NULL || !helper_parse_shell_plan(script, &plan)) {
		return 1;
	}
	for (int i = 0; i < plan.count; i++) {
		helper_node *node = &plan.nodes[i];
		if (node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
			continue;
		}
		const char *tool = base_name(node->argv[0]);
		if (tool == NULL) {
			return 1;
		}
		if (strcmp(tool, "printenv") == 0 ||
		    strcmp(tool, "ls") == 0 ||
		    strcmp(tool, "file") == 0 ||
		    strcmp(tool, "grep") == 0 ||
		    strcmp(tool, "rg") == 0 ||
		    strcmp(tool, "sort") == 0) {
			return 1;
		}
	}
	return 0;
}

static int squire_hot_env_lookup(int envc, const char *const *env, const char *key, const char **value) {
	if (value != NULL) {
		*value = NULL;
	}
	if (envc <= 0 || env == NULL || key == NULL) {
		return 0;
	}
	size_t key_len = strlen(key);
	for (int i = 0; i < envc; i++) {
		const char *entry = env[i];
		if (entry != NULL && strncmp(entry, key, key_len) == 0 && entry[key_len] == '=') {
			if (value != NULL) {
				*value = entry + key_len + 1;
			}
			return 1;
		}
	}
	return 0;
}

static int squire_hot_process_env_lookup(const char *key, const char **value) {
	if (value != NULL) {
		*value = NULL;
	}
	if (environ == NULL || key == NULL) {
		return 0;
	}
	size_t key_len = strlen(key);
	for (size_t i = 0; environ[i] != NULL; i++) {
		const char *entry = environ[i];
		if (strncmp(entry, key, key_len) == 0 && entry[key_len] == '=') {
			if (value != NULL) {
				*value = entry + key_len + 1;
			}
			return 1;
		}
	}
	return 0;
}

static int squire_hot_env_values_match(int envc, const char *const *env, const char *key) {
	const char *actual = NULL;
	const char *expected = NULL;
	int actual_found = squire_hot_process_env_lookup(key, &actual);
	int expected_found = squire_hot_env_lookup(envc, env, key, &expected);
	return actual_found == expected_found &&
	       (!actual_found || strcmp(actual, expected) == 0);
}

static int squire_hot_env_key_compatible(int envc, const char *const *env, const char *key, int (*selected)(const char *, size_t)) {
	size_t key_len = strlen(key);
	if (!selected(key, key_len)) {
		return 1;
	}
	if (squire_hot_env_values_match(envc, env, key)) {
		return 1;
	}
	char msg[256];
	const char *actual = NULL;
	const char *expected = NULL;
	int actual_found = squire_hot_process_env_lookup(key, &actual);
	int expected_found = squire_hot_env_lookup(envc, env, key, &expected);
	snprintf(msg, sizeof(msg), "env mismatch key=%s actual=%s expected=%s", key, actual_found ? (actual[0] == '\0' ? "<empty>" : "<set>") : "<unset>", expected_found ? (expected[0] == '\0' ? "<empty>" : "<set>") : "<unset>");
	squire_hot_trace(msg);
	return 0;
}

static int squire_hot_env_compatible_for(int envc, const char *const *env, int (*selected)(const char *, size_t)) {
	if (envc < 0 || (envc > 0 && env == NULL)) {
		return 0;
	}
	for (int i = 0; i < envc; i++) {
		const char *entry = env[i];
		if (entry == NULL) {
			return 0;
		}
		const char *equals = strchr(entry, '=');
		if (equals == NULL) {
			return 0;
		}
		size_t key_len = (size_t)(equals - entry);
		if (!selected(entry, key_len)) {
			continue;
		}
		char key[128];
		if (key_len == 0 || key_len >= sizeof(key)) {
			return 0;
		}
		memcpy(key, entry, key_len);
		key[key_len] = '\0';
		if (!squire_hot_env_key_compatible(envc, env, key, selected)) {
			return 0;
		}
	}
	if (environ != NULL) {
		for (size_t i = 0; environ[i] != NULL; i++) {
			const char *entry = environ[i];
			const char *equals = strchr(entry, '=');
			size_t key_len = equals == NULL ? strlen(entry) : (size_t)(equals - entry);
			if (key_len == 0 || key_len >= 128 || !selected(entry, key_len)) {
				continue;
			}
			char key[128];
			memcpy(key, entry, key_len);
			key[key_len] = '\0';
			if (!squire_hot_env_key_compatible(envc, env, key, selected)) {
				return 0;
			}
		}
	}
	return 1;
}

static int squire_hot_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_sensitive_env_key);
}

static int squire_hot_all_env_key(const char *entry, size_t key_len) {
	(void)entry;
	return key_len > 0;
}

static int squire_hot_full_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_all_env_key);
}

static int squire_hot_git_metadata_env_compatible(int envc, const char *const *env) {
	return squire_hot_env_compatible_for(envc, env, squire_hot_git_metadata_env_key);
}

static int squire_hot_shell_script_env_compatible(const char *script, int envc, const char *const *env) {
	if (squire_hot_shell_script_requires_full_env(script)) {
		return squire_hot_env_compatible(envc, env);
	}
	return squire_hot_env_compatible_for(envc, env, squire_hot_shell_script_env_key);
}

static int squire_hot_shell_script_index(int argc, const char *const *argv) {
	if (argc < 3 || argv == NULL || argv[0] == NULL) {
		return -1;
	}
	const char *shell = base_name(argv[0]);
	if (shell == NULL ||
	    (strcmp(shell, "sh") != 0 && strcmp(shell, "bash") != 0 && strcmp(shell, "zsh") != 0)) {
		return -1;
	}
	if (argv[1] != NULL && (strcmp(argv[1], "-c") == 0 || strcmp(argv[1], "-lc") == 0)) {
		return 2;
	}
	if (argc >= 4 && argv[1] != NULL && argv[2] != NULL &&
	    strcmp(argv[1], "-l") == 0 && strcmp(argv[2], "-c") == 0) {
		return 3;
	}
	return -1;
}

static int squire_hot_direct_requires_full_env(int argc, const char *const *argv) {
	if (argc <= 0 || argv == NULL || argv[0] == NULL) {
		return 1;
	}
	const char *tool = base_name(argv[0]);
	return tool != NULL &&
	       (strcmp(tool, "printenv") == 0 ||
	        strcmp(tool, "ls") == 0 ||
	        strcmp(tool, "file") == 0 ||
	        strcmp(tool, "grep") == 0 ||
	        strcmp(tool, "rg") == 0);
}

static int squire_hot_is_git_metadata_argv(int argc, const char *const *argv) {
	if (argv == NULL || argc < 3 || argv[0] == NULL || argv[1] == NULL || argv[2] == NULL) {
		return 0;
	}
	if (argc == 3 &&
	    strcmp(base_name(argv[0]), "git") == 0 &&
	    strcmp(argv[1], "branch") == 0 &&
	    strcmp(argv[2], "--show-current") == 0) {
		return 1;
	}
	if (strcmp(base_name(argv[0]), "git") != 0 || strcmp(argv[1], "rev-parse") != 0) {
		return 0;
	}
	if (argc == 3) {
		return strcmp(argv[2], "HEAD") == 0 ||
		       strcmp(argv[2], "--git-dir") == 0 ||
		       strcmp(argv[2], "--show-toplevel") == 0 ||
		       strcmp(argv[2], "--is-inside-work-tree") == 0;
	}
	return argc == 4 && argv[3] != NULL &&
	       strcmp(argv[2], "--abbrev-ref") == 0 &&
	       strcmp(argv[3], "HEAD") == 0;
}

static int squire_hot_is_git_metadata_script(const char *script) {
	helper_plan plan;
	if (script == NULL || !helper_parse_shell_plan(script, &plan) || plan.root < 0 || plan.root >= plan.count) {
		return 0;
	}
	helper_node *node = &plan.nodes[plan.root];
	if (node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
		return 0;
	}
	const char *argv[HELPER_SHELL_MAX_ARGS];
	for (int i = 0; i < node->argc; i++) {
		argv[i] = node->argv[i];
	}
	return squire_hot_is_git_metadata_argv(node->argc, argv);
}

static int squire_runtime_direct_candidate(const char *cwd, int argc, const char *const *argv) {
	if (cwd == NULL || argv == NULL || argc <= 0 || argc > MAX_ARGC) {
		return 0;
	}
	char *mutable_argv[MAX_ARGC];
	for (int i = 0; i < argc; i++) {
		if (argv[i] == NULL) {
			return 0;
		}
		mutable_argv[i] = (char *)argv[i];
	}
	policy_invocation inv;
	if (!normalize_invocation_at_cwd(cwd, argc, mutable_argv, &inv)) {
		return 0;
	}
	char target[PATH_BUF], flag[16];
	int static_environment = is_static_environment_probe(&inv) &&
	                         (strcmp(inv.argv[0], "hostname") == 0 || strcmp(inv.argv[0], "uname") == 0);
	return is_git_metadata(&inv) ||
	       is_git_ls_files(&inv) ||
	       is_git_status(&inv) ||
	       is_git_head_subject_log(&inv) ||
	       is_git_read_only_diff(&inv) ||
	       static_environment ||
	       is_printenv_probe(&inv) ||
	       parse_directory_listing(&inv, target, flag) ||
	       is_warm_file_candidate(&inv);
}

static int squire_runtime_filter_candidate(helper_node *node) {
	if (node == NULL || node->kind != HELPER_NODE_EXEC || node->argc <= 0) {
		return 0;
	}
	const char *name = base_name(node->argv[0]);
	if (name == NULL) {
		return 0;
	}
	int count = 0;
	const char *pattern = NULL;
	int quiet = 0;
	return (strcmp(name, "cat") == 0 && node->argc == 1) ||
	       (strcmp(name, "head") == 0 && helper_parse_line_count_arg(node, &count)) ||
	       (strcmp(name, "tail") == 0 && helper_parse_line_count_arg(node, &count)) ||
	       (strcmp(name, "grep") == 0 && helper_parse_stdin_grep(node->argc, node->argv, &pattern, &quiet)) ||
	       (strcmp(name, "wc") == 0 && node->argc == 2 && strcmp(node->argv[1], "-l") == 0) ||
	       (strcmp(name, "sort") == 0 && node->argc == 1);
}

static int squire_runtime_plan_candidate(const char *cwd, helper_plan *plan, int idx, int has_input) {
	if (plan == NULL || idx < 0 || idx >= plan->count) {
		return 0;
	}
	helper_node *node = &plan->nodes[idx];
	switch (node->kind) {
	case HELPER_NODE_EXEC:
		if (has_input) {
			return squire_runtime_filter_candidate(node);
		}
		{
			const char *argv[HELPER_SHELL_MAX_ARGS];
			for (int i = 0; i < node->argc; i++) {
				argv[i] = node->argv[i];
			}
			return squire_runtime_direct_candidate(cwd, node->argc, argv);
		}
	case HELPER_NODE_PIPE:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input) &&
		       squire_runtime_plan_candidate(cwd, plan, node->right, 1);
	case HELPER_NODE_AND:
	case HELPER_NODE_SEQ:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input) &&
		       squire_runtime_plan_candidate(cwd, plan, node->right, has_input);
	case HELPER_NODE_REDIR_NULL:
		return squire_runtime_plan_candidate(cwd, plan, node->left, has_input);
	default:
		return 0;
	}
}

static int squire_runtime_candidate(const char *cwd, int argc, const char *const *argv) {
	int script_idx = squire_hot_shell_script_index(argc, argv);
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		helper_plan plan;
		return helper_parse_shell_plan(argv[script_idx], &plan) &&
		       squire_runtime_plan_candidate(cwd, &plan, plan.root, 0);
	}
	return squire_runtime_direct_candidate(cwd, argc, argv);
}

int squire_hot_try_replay_command(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_hot_result *out) {
	if (out == NULL || argc <= 0 || argv == NULL) {
		return 0;
	}
	memset(out, 0, sizeof(*out));
	int script_idx = squire_hot_shell_script_index(argc, argv);
	int git_metadata = squire_hot_is_git_metadata_argv(argc, argv) ||
	                   (script_idx >= 0 && script_idx < argc && squire_hot_is_git_metadata_script(argv[script_idx]));
	if (git_metadata) {
		if (!squire_hot_git_metadata_env_compatible(envc, env)) {
			squire_hot_trace("miss git-metadata-env-incompatible");
			return 0;
		}
	} else if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		int env_ok = squire_hot_shell_script_requires_full_env(argv[script_idx])
		             ? squire_hot_full_env_compatible(envc, env)
		             : squire_hot_shell_script_env_compatible(argv[script_idx], envc, env);
		if (!env_ok) {
			squire_hot_trace("miss shell-script-env-incompatible");
			return 0;
		}
	} else if (squire_hot_direct_requires_full_env(argc, argv)) {
		if (!squire_hot_full_env_compatible(envc, env)) {
			squire_hot_trace("miss full-env-incompatible");
			return 0;
		}
	} else if (!squire_hot_env_compatible(envc, env)) {
		squire_hot_trace("miss env-incompatible");
		return 0;
	}
	if (script_idx >= 0 && script_idx < argc && argv[script_idx] != NULL) {
		squire_hot_trace("try shell script");
		return squire_hot_try_replay_script(cwd, argv[script_idx], out);
	}
	squire_hot_trace("try argv");
	return squire_hot_try_replay_argv(cwd, argc, argv, out);
}

uint32_t squire_runtime_abi_version(void) {
	return SQUIRE_RUNTIME_ABI_VERSION;
}

int squire_runtime_try_execute(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_runtime_result *out) {
	if (!squire_runtime_candidate(cwd, argc, argv)) {
		if (out != NULL) {
			memset(out, 0, sizeof(*out));
		}
		return SQUIRE_RUNTIME_UNSUPPORTED;
	}
	return squire_hot_try_replay_command(cwd, argc, argv, envc, env, out) == 1
	       ? SQUIRE_RUNTIME_HIT
	       : SQUIRE_RUNTIME_MISS;
}

void squire_hot_record_replay(squire_hot_result *result) {
	if (result == NULL || result->handle == NULL) {
		return;
	}
	squire_hot_handle *handle = (squire_hot_handle *)result->handle;
	if (handle->kind == SQUIRE_HOT_HANDLE_EXACT) {
		record_hot_replay_event(handle->exact.store_root, (long long)handle->exact.native_wall_ms, handle->exact.replay_start_ns);
	}
}

void squire_runtime_record_hit(squire_runtime_result *result) {
	squire_hot_record_replay(result);
}

void squire_hot_release(squire_hot_result *result) {
	if (result == NULL || result->handle == NULL) {
		return;
	}
	squire_hot_handle *handle = (squire_hot_handle *)result->handle;
	if (handle->kind == SQUIRE_HOT_HANDLE_EXACT) {
		release_prepared_exact_replay(&handle->exact);
	} else if (handle->kind == SQUIRE_HOT_HANDLE_SCRIPT) {
		helper_result_free(&handle->script);
	}
	free(handle);
	memset(result, 0, sizeof(*result));
}

void squire_runtime_release(squire_runtime_result *result) {
	squire_hot_release(result);
}
