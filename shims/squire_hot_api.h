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

int squire_hot_try_replay_argv(const char *cwd, int argc, const char *const *argv, squire_hot_result *out);
int squire_hot_try_replay_script(const char *cwd, const char *script, squire_hot_result *out);
int squire_hot_try_replay_command(const char *cwd, int argc, const char *const *argv, int envc, const char *const *env, squire_hot_result *out);
void squire_hot_record_replay(squire_hot_result *result);
void squire_hot_release(squire_hot_result *result);

#ifdef __cplusplus
}
#endif

#endif
