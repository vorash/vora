package seccomp

// IPCSyscalls are denied in a sandbox that will be reset. It keeps its IPC namespace for
// life, so a SysV object or a POSIX message queue left by one run is there for the next,
// and a reset, which works on files and processes, can't see them.
//
// For SysV the three create calls are enough. The namespace starts empty and nothing else
// can add to it, so every other SysV call has no valid id to act on and fails with EINVAL.
//
// This list has no build tag because the gVisor backend passes the names to runsc.
// ipcSyscalls holds the same calls as numbers.
var IPCSyscalls = []string{
	"shmget", "semget", "msgget",
	"mq_open", "mq_unlink", "mq_timedsend", "mq_timedreceive", "mq_notify", "mq_getsetattr",
}
