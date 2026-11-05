"""Validate HAP archives and the executable runtime packaging contract."""
import argparse
import hashlib
import json
from pathlib import Path
import stat
from pathlib import PurePosixPath

ADAPTER = Path(__file__).resolve().parents[1]


def extract_payload(archive, destination):
    """Validate every path/type before extracting into a new owned directory."""
    destination = Path(destination)
    if destination.exists():
        raise ValueError('Archive destination must be a new directory')
    seen = set()
    for entry in archive.infolist():
        raw = entry.filename
        name = PurePosixPath(raw)
        mode = entry.external_attr >> 16
        if ('\0' in entry.orig_filename or '\\' in raw or name.is_absolute()
                or not raw.rstrip('/') or any(p in ('', '.', '..') for p in raw.rstrip('/').split('/'))):
            raise ValueError('Unsafe archive path')
        if str(name) in seen:
            raise ValueError('Duplicate archive path')
        seen.add(str(name))
        if stat.S_IFMT(mode) not in (0, stat.S_IFREG, stat.S_IFDIR) or mode & (stat.S_ISUID | stat.S_ISGID | stat.S_ISVTX):
            raise ValueError('Unsupported archive type or privileged mode')
        if not (destination / raw).resolve().is_relative_to(destination.resolve()):
            raise ValueError('Archive path escapes destination')
    archive.extractall(destination)
    for entry in archive.infolist():
        mode = entry.external_attr >> 16
        if mode:
            (destination / entry.filename).chmod(stat.S_IMODE(mode))


def check_shutdown_contract(native, identity):
    """Do not pair the new native exit predicate with an old Go APP."""
    required = any(name.endswith('/libollamabridge.so') and b'appShutdownStatus' in data
                   for name, data in native.items())
    if not required:
        return
    if not identity:
        raise ValueError('Shutdown ACK HAP requires --runtime with a verified matching Go APP')
    adapters = identity.get('source', {}).get('adapters', {})
    for name in ['main.go', 'shutdown_harmony.go', 'services_harmony.go']:
        expected = hashlib.sha256((ADAPTER/'app'/name).read_bytes()).hexdigest()
        if adapters.get(name) != expected:
            raise ValueError('Shutdown ACK runtime source mismatch: '+name)
    if any(name.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data for name,data in native.items()):
        for name in ['parent_lifeline_harmony.go','wait_group_harmony.go','wait_executable_harmony.go','desired_elf_harmony.go']:
            if adapters.get(name)!=hashlib.sha256((ADAPTER/'app'/name).read_bytes()).hexdigest():
                raise ValueError('ELF supervision runtime source mismatch: '+name)
        if identity.get('source',{}).get('core_wait_patch_sha256')!=hashlib.sha256((ADAPTER/'core-wait.patch').read_bytes()).hexdigest():
            raise ValueError('ELF core restart drain hook missing/mismatched')
        if identity.get('source',{}).get('frontend_contract') is not True or not identity['source'].get('frontend_dist'):
            raise ValueError('ELF runtime requires rebuilt and qualified embedded frontend')


def check_embedded_frontend(data):
    markers=[b'Choose custom storage folder',b'Use default private folder',b'Storage mode:']
    if not all(marker in data for marker in markers) or b'Choose migration parent folder' in data:
        raise ValueError('stale or missing embedded frontend bytes')


def check_elf_build_mode(native, record):
    bridges=[data for name,data in native.items() if name.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data]
    if not bridges:return
    mode=json.loads(native['module.json'])['app']['buildMode']
    if mode!=record.get('mode') or mode not in ['debug','release']:
        raise ValueError('ELF SDK/manifest build mode mismatch')
    disabled=b'diagnostic commands disabled in release' in bridges[0]
    debug=b'debug APP crash requested' in bridges[0]
    if (disabled,debug)!=((False,True) if mode=='debug' else (True,False)):
        raise ValueError('ELF actual Native build mode mismatch; stale SDK object')


def check_elf_native_identity(native, record):
    if any(n.endswith('/libollamabridge.so') and b'PICO_PARENT_LIFELINE_FD=' in data for n,data in native.items()):
        parts=['bridge.cpp','elf_supervision.h','storage_permission_audit.cpp','storage_permission_audit.h','CMakeLists.txt']
        expected=hashlib.sha256(''.join(record['inputs']['entry/src/main/cpp/'+part] for part in parts).encode('ascii')).hexdigest().encode('ascii')
        if not all(expected in data for n,data in native.items() if n.endswith('/libollamabridge.so')):
            raise ValueError('ELF native library does not embed the SDK-recorded bridge source identity')
