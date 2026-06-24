// Internal helper for Squire's scoped preload transport.
//
// The preload library uses this executable when an intercepted posix_spawn call
// includes file actions, such as stdout/stderr pipes. The caller's native
// posix_spawn applies those file actions to this helper; the helper then writes
// exact replay bytes to the already-wired descriptors or execs the native
// command on any miss. It is not a PATH shim and is not invoked by agents.

#define SQUIRE_MMAP_NO_MAIN 1
#define SQUIRE_MMAP_HELPER_REAL_EXEC 1
#include "squire_mmap_shim.c"

int main(int argc, char **argv) {
	if (argc < 2) {
		fprintf(stderr, "squire preload helper: missing command\n");
		return 127;
	}
	int command_argc = argc - 1;
	char **command_argv = &argv[1];
	if (!try_replay(command_argc, command_argv)) {
		if (getenv("SQUIRE_SHIM_REQUIRE_HIT") != NULL) {
			fprintf(stderr, "squire preload helper: hot snapshot miss\n");
			return 91;
		}
		exec_real_command(command_argc, command_argv);
	}
	return 0;
}
