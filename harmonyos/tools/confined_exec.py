"""Linux/x86_64 host-test guard: private network, no capabilities, confined writes.

Invoke inside a freshly created user+network namespace; never use this helper to
change the host network. The parent explicitly lists writable test/cache paths.
"""
import ctypes
import json
import os
from pathlib import Path
import platform
import subprocess
import sys


def confine(paths):
    if platform.system() != 'Linux' or platform.machine() != 'x86_64':
        raise RuntimeError('Landlock syscall mapping is qualified only on Linux/x86_64')
    libc = ctypes.CDLL(None, use_errno=True)
    abi = libc.syscall(444, 0, 0, 1)
    if abi < 3:
        raise RuntimeError('Landlock ABI >= 3 required')
    rights = sum(1 << index for index in (1, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14))

    class Ruleset(ctypes.Structure):
        _fields_ = [('handled_access_fs', ctypes.c_uint64)]

    class Beneath(ctypes.Structure):
        _pack_ = 1
        _fields_ = [('allowed_access', ctypes.c_uint64), ('parent_fd', ctypes.c_int32)]

    attr = Ruleset(rights)
    descriptor = libc.syscall(444, ctypes.byref(attr), ctypes.sizeof(attr), 0)
    if descriptor < 0:
        raise OSError(ctypes.get_errno(), 'landlock_create_ruleset')
    try:
        for name in paths:
            path = Path(name)
            if not path.is_absolute() or not path.exists() or path.is_symlink():
                raise ValueError('Writable paths must be explicit, existing, unlinked absolute paths')
            fd = os.open(path, os.O_PATH | os.O_CLOEXEC)
            try:
                access = rights if path.is_dir() else (1 << 1) | (1 << 14)
                rule = Beneath(access, fd)
                if libc.syscall(445, descriptor, 1, ctypes.byref(rule), 0):
                    raise OSError(ctypes.get_errno(), 'landlock_add_rule')
            finally:
                os.close(fd)
        if libc.prctl(38, 1, 0, 0, 0) or libc.syscall(446, descriptor, 0):
            raise OSError(ctypes.get_errno(), 'landlock_restrict_self')
    finally:
        os.close(descriptor)


def main():
    arguments = sys.argv[1:]
    split = arguments.index('--')
    # unshare must have created a distinct network namespace before reaching us.
    parent_namespace = int(os.environ.pop('PICO_TEST_PARENT_NETNS'))
    if parent_namespace <= 0 or os.stat('/proc/self/ns/net').st_ino == parent_namespace:
        raise RuntimeError('Refusing to configure loopback in the host network namespace')
    subprocess.run(['/usr/sbin/ip', 'link', 'set', 'lo', 'up'], check=True)
    libc = ctypes.CDLL(None, use_errno=True)

    class Header(ctypes.Structure):
        _fields_ = [('version', ctypes.c_uint32), ('pid', ctypes.c_int)]

    class Caps(ctypes.Structure):
        _fields_ = [('effective', ctypes.c_uint32), ('permitted', ctypes.c_uint32), ('inheritable', ctypes.c_uint32)]

    header = Header(0x20080522, 0)
    caps = (Caps * 2)()
    if libc.capset(ctypes.byref(header), ctypes.byref(caps)):
        raise OSError(ctypes.get_errno(), 'capset')
    status = Path('/proc/self/status').read_text()
    capability = next(line.split()[1] for line in status.splitlines() if line.startswith('CapEff:'))
    if int(capability, 16):
        raise RuntimeError('Capabilities remained enabled')
    confine(arguments[:split])
    print(json.dumps({'host_test_guard': True, 'uid': os.getuid(), 'effective_capabilities': capability,
                      'network_namespace': os.stat('/proc/self/ns/net').st_ino}), flush=True)
    os.execvpe(arguments[split + 1], arguments[split + 1:], os.environ)


if __name__ == '__main__':
    main()
