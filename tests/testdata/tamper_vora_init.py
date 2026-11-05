# Run inside a sandbox: every way a workload might replace or change the init
# binary. Shared by the bwrap security test and the gVisor integration test, so
# both backends face the same attempts.
#
# Prints "<attempt> <errno name>" per attempt, or "<attempt> OK" if it worked,
# then the binary's hash. Exits 1 if an attempt worked or failed for a reason
# that does not show the binary is protected: ENOENT would mean this script is
# looking at the wrong path, not that the sandbox held.
import errno
import hashlib
import os
import sys

TARGET = "/vora/vora-init"
# ETXTBSY: a plain write open of a running binary. The kernel takes the write
# deny before the read-only mount check, so this says nothing about the bind;
# the rename, unlink and chmod attempts are what tell the bind apart.
PROTECTED = {errno.EROFS, errno.EBUSY, errno.ETXTBSY, errno.EXDEV}


def overwrite():
    fd = os.open(TARGET, os.O_WRONLY)
    try:
        os.write(fd, b"tampered")
    finally:
        os.close(fd)


def truncate_open():
    os.close(os.open(TARGET, os.O_WRONLY | os.O_TRUNC))


# A file to rename over the binary. bwrap keeps /vora writable for the control
# socket; under gVisor the create fails, so the source comes from /tmp instead.
source = "/vora/evil"
try:
    with open(source, "wb") as f:
        f.write(b"tampered")
    print("create OK")
except OSError as e:
    print("create", errno.errorcode[e.errno])
    source = "/tmp/evil"
    with open(source, "wb") as f:
        f.write(b"tampered")

attempts = [
    ("overwrite", overwrite),
    ("truncate-open", truncate_open),
    ("truncate", lambda: os.truncate(TARGET, 0)),
    ("chmod", lambda: os.chmod(TARGET, 0o777)),
    # A second name on the writable mount would give a writable path to the inode.
    ("link", lambda: os.link(TARGET, "/vora/linked")),
    ("rename-over", lambda: os.rename(source, TARGET)),
    ("rename-away", lambda: os.rename(TARGET, "/vora/moved")),
    ("unlink", lambda: os.unlink(TARGET)),
]

failed = False
for name, attempt in attempts:
    try:
        attempt()
    except OSError as e:
        print(name, errno.errorcode[e.errno])
        failed = failed or e.errno not in PROTECTED
    else:
        print(name, "OK")
        failed = True

with open(TARGET, "rb") as f:
    print("sha256", hashlib.sha256(f.read()).hexdigest())

sys.exit(1 if failed else 0)
