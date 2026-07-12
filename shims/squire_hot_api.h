#ifndef SQUIRE_HOT_API_H
#define SQUIRE_HOT_API_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct squire_hot_result {
	void *handle;
	const unsigned char *stdout_data;
	uint32_t stdout_len;
	const unsigned char *stderr_data;
	uint32_t stderr_len;
	int exit_code;
	uint64_t native_wall_ms;
} squire_hot_result;

#define SQUIRE_RUNTIME_ABI_VERSION 1u
#define SQUIRE_RUNTIME_UNSUPPORTED (-1)
#define SQUIRE_RUNTIME_MISS 0
#define SQUIRE_RUNTIME_HIT 1

typedef squire_hot_result squire_runtime_result;

uint32_t squire_runtime_abi_version(void);
int squire_runtime_try_execute(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_runtime_result *out);
void squire_runtime_record_hit(squire_runtime_result *result);
void squire_runtime_release(squire_runtime_result *result);

int squire_hot_try_replay_argv(const char *cwd, int argc, const char *const *argv, squire_hot_result *out);
int squire_hot_try_replay_script(const char *cwd, const char *script, squire_hot_result *out);
int squire_hot_try_replay_command(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_hot_result *out);
void squire_hot_record_replay(squire_hot_result *result);
void squire_hot_release(squire_hot_result *result);

#ifdef __cplusplus
}
#endif

#endif
