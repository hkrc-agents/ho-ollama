"""Host fake-DSO tests of the unchanged production NDK audit (not device proof)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


HEADER = r"""
#pragma once
#include <stdbool.h>
#ifdef __cplusplus
extern "C" {
#endif
typedef int FileManagement_ErrCode;
enum { READ_MODE=1, WRITE_MODE=2 };
typedef struct { char *uri; unsigned int length, operationMode; } FileShare_PolicyInfo;
typedef struct { char *uri; int code; char *message; } FileShare_PolicyErrorResult;
int OH_FileShare_CheckPersistentPermission(const FileShare_PolicyInfo*,unsigned int,bool**,unsigned int*);
int OH_FileShare_PersistPermission(const FileShare_PolicyInfo*,unsigned int,FileShare_PolicyErrorResult**,unsigned int*);
int OH_FileShare_ActivatePermission(const FileShare_PolicyInfo*,unsigned int,FileShare_PolicyErrorResult**,unsigned int*);
void OH_FileShare_ReleasePolicyErrorResult(FileShare_PolicyErrorResult*,unsigned int);
bool OH_AT_CheckSelfPermission(const char*);
#ifdef __cplusplus
}
#endif
"""

FAKE = r"""
#include "filemanagement/fileshare/oh_file_share.h"
#include <stdlib.h>
#include <string.h>
extern void AuditTestEvent(char);
extern void AuditTestAllocated(void*,int);
extern void AuditTestReleased(void*,unsigned int);
static int checks;
static int mode(const char *s) { const char *v=getenv("AUDIT_TEST_MODE"); return v && !strcmp(v,s); }
static void input(const FileShare_PolicyInfo *p,unsigned int n) {
    if (!p || n!=1 || !p->uri || p->length!=strlen(p->uri) || p->operationMode!=3) abort();
    const char *expected=getenv("AUDIT_TEST_URI");
    if (!expected || strcmp(expected,p->uri)) abort();
}
#ifndef OMIT_CHECK
int OH_FileShare_CheckPersistentPermission(const FileShare_PolicyInfo *p,unsigned int n,bool **v,unsigned int *count) {
    input(p,n); AuditTestEvent('C'); ++checks;
    *count=mode("bad_count")?2:1;
    if (mode("null_pointer")) { *v=NULL; return 0; }
    *v=malloc(*count*sizeof(bool)); if(!*v)abort(); AuditTestAllocated(*v,1);
    for(unsigned int i=0;i<*count;++i) (*v)[i]=!mode("false") && (mode("after_false")?checks==1:checks>=2);
    return mode("error_check")?201:0;
}
#endif
static int apply(char event,const FileShare_PolicyInfo *p,unsigned int n,FileShare_PolicyErrorResult **v,unsigned int *count) {
    input(p,n); AuditTestEvent(event); *v=NULL; *count=0;
    if(mode("policy_errors")||mode("big_error_count")) {
        *count=mode("big_error_count")?17:1;
        *v=calloc(*count,sizeof(**v));if(!*v)abort();AuditTestAllocated(*v,2);
        for(unsigned int i=0;i<*count;++i) { (*v)[i].code=event=='P'?1:4; (*v)[i].uri="DO_NOT_ECHO_URI"; (*v)[i].message="DO_NOT_ECHO_MESSAGE"; }
        return 13900001;
    }
    return 0;
}
#ifndef OMIT_PERSIST
int OH_FileShare_PersistPermission(const FileShare_PolicyInfo *p,unsigned int n,FileShare_PolicyErrorResult **v,unsigned int *count) {return apply('P',p,n,v,count);}
#endif
#ifndef OMIT_ACTIVATE
int OH_FileShare_ActivatePermission(const FileShare_PolicyInfo *p,unsigned int n,FileShare_PolicyErrorResult **v,unsigned int *count) {return apply('A',p,n,v,count);}
#endif
#ifndef OMIT_RELEASE
void OH_FileShare_ReleasePolicyErrorResult(FileShare_PolicyErrorResult *p,unsigned int count) {
    AuditTestEvent('R'); AuditTestReleased(p,count); free(p);
}
#endif
"""

ACCESS = r"""
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>
extern void AuditTestEvent(char);
bool OH_AT_CheckSelfPermission(const char *p) {
    if(strcmp(p,"ohos.permission.FILE_ACCESS_PERSIST"))abort();
    AuditTestEvent('S');return true;
}
"""

DRIVER = r"""
#include "storage_permission_audit.h"
#include <cstdlib>
#include <iostream>
#include <iterator>
#include <unistd.h>
static void *pointers[64];
static int kinds[64],total,allocations,frees,errorAllocations,releases,wrong;
static char events[128];static int eventCount;
extern "C" void AuditTestEvent(char c) { if(eventCount>=127)std::abort();events[eventCount++]=c; }
extern "C" void AuditTestAllocated(void *p,int kind) {
    if(total>=64)std::abort();
    pointers[total]=p;kinds[total++]=kind;
    if(kind==1)++allocations;else ++errorAllocations;
}
extern "C" void AuditTestReleased(void *p,unsigned int count) {
    const char *mode=std::getenv("AUDIT_TEST_MODE");
    const unsigned int expected=mode && std::string(mode)=="big_error_count"?17:1;
    if(count!=expected)std::abort();
    for(int i=total-1;i>=0;--i)if(pointers[i]==p){if(kinds[i]!=2)++wrong;pointers[i]=nullptr;++releases;return;}
    ++wrong;
}
extern "C" void __real_free(void*);
extern "C" void __wrap_free(void *p) {
    if(p)for(int i=total-1;i>=0;--i)if(pointers[i]==p){if(kinds[i]!=1)++wrong;else ++frees;pointers[i]=nullptr;break;}
    __real_free(p);
}
int main() {
    const std::string uri(std::istreambuf_iterator<char>(std::cin),{});
    const std::string result=AuditStoragePermission(uri);
    int outstanding=0;for(int i=0;i<total;++i)if(pointers[i])++outstanding;
    std::cout<<result<<'\n'<<"{\"trace\":\""<<events<<"\",\"allocated\":"<<allocations
        <<",\"freed\":"<<frees<<",\"error_allocated\":"<<errorAllocations<<",\"released\":"<<releases
        <<",\"wrong_free\":"<<wrong<<",\"outstanding\":"<<outstanding
        <<",\"pid\":"<<getpid()<<",\"uid\":"<<getuid()<<"}\n";
}
"""


class StoragePermissionAuditTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.cc = shutil.which("cc")
        cls.cxx = shutil.which("g++")
        if not cls.cc or not cls.cxx:
            raise unittest.SkipTest("host cc and g++ required")
        cls.temporary = tempfile.TemporaryDirectory(prefix="pico-permission-audit-test-")
        cls.addClassCleanup(cls.temporary.cleanup)
        cls.tmp = Path(cls.temporary.name)
        cls.production = Path(__file__).resolve().parents[1] / "hap/entry/src/main/cpp"
        for name in ("filemanagement/fileshare/oh_file_share.h", "accesstoken/ability_access_control.h"):
            path = cls.tmp / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(HEADER)
        (cls.tmp / "driver.cpp").write_text(DRIVER)
        (cls.tmp / "fake.c").write_text(FAKE)
        (cls.tmp / "access.c").write_text(ACCESS)
        cls.binary = cls.tmp / "audit-test"
        cls.compile([
            cls.cxx, "-std=c++17", "-Wall", "-Wextra", "-Werror", "-I", str(cls.tmp),
            "-I", str(cls.production), str(cls.production / "storage_permission_audit.cpp"),
            str(cls.tmp / "driver.cpp"), "-rdynamic", "-Wl,--wrap=free", "-ldl", "-o", str(cls.binary),
        ])
        cls.variants = {}
        for variant in ("full", "missing_library", "missing_access", "OMIT_CHECK", "OMIT_PERSIST", "OMIT_ACTIVATE", "OMIT_RELEASE"):
            directory = cls.tmp / variant
            directory.mkdir()
            cls.variants[variant] = directory
            common = [cls.cc, "-shared", "-fPIC", "-Wall", "-Wextra", "-Werror", "-I", str(cls.tmp)]
            if variant != "missing_library":
                defines = ["-D" + variant] if variant.startswith("OMIT_") else []
                # Some helpers are intentionally unused when a public symbol is absent.
                cls.compile(common + ["-Wno-unused-function", "-Wno-unused-variable"] + defines + [
                    str(cls.tmp / "fake.c"), "-o", str(directory / "libohfileshare.so"),
                ])
            if variant != "missing_access":
                cls.compile(common + [str(cls.tmp / "access.c"), "-o", str(directory / "libability_access_control.so")])

    @staticmethod
    def compile(command):
        completed = subprocess.run(command, capture_output=True, text=True, timeout=30)
        if completed.returncode:
            raise RuntimeError(completed.stdout + completed.stderr)

    def audit(self, mode="false", variant="full", uri="file:///chosen-only"):
        env = os.environ.copy()
        env.pop("LD_PRELOAD", None)
        env.pop("LD_AUDIT", None)
        env.update(LD_LIBRARY_PATH=str(self.variants[variant]), AUDIT_TEST_MODE=mode, AUDIT_TEST_URI=uri)
        # NUL input cannot be put in env, but production accepts a std::string
        # from stdin and must reject it before invoking any fake policy API.
        if "\0" in uri:
            env["AUDIT_TEST_URI"] = "unused-invalid-input"
        completed = subprocess.run([str(self.binary)], input=uri, env=env, capture_output=True, text=True, check=True, timeout=5)
        lines = completed.stdout.splitlines()
        self.assertEqual(len(lines), 2, completed.stdout)
        result, stats = map(json.loads, lines)
        self.assertLessEqual(len(lines[0].encode()), 32768)
        self.assertEqual(stats["outstanding"], 0)
        self.assertEqual(stats["wrong_free"], 0)
        self.assertEqual(stats["allocated"], stats["freed"])
        self.assertEqual(stats["error_allocated"], stats["released"])
        self.assertEqual(result["uid"], stats["uid"])
        self.assertEqual(result["pid"], stats["pid"])
        self.assertIsInstance(result["domain"], str)
        self.assertIsInstance(result["domain_errno"], int)
        self.assertEqual(result["mode"], 3)
        self.assertNotIn("DO_NOT_ECHO", lines[0])
        return result, stats

    def test_success_return_without_persistence_never_activates(self):
        result, stats = self.audit("false")
        self.assertEqual(stats["trace"], "SCPCC")
        self.assertEqual(result["persist"]["rc"], 0)
        self.assertEqual(result["check_after"]["value"], 0)
        self.assertEqual(result["activation_eligible"], 0)
        self.assertEqual(result["activate"]["called"], 0)
        self.assertEqual(result["check_final"]["value"], 0)
        self.assertEqual(stats["freed"], 3)

    def test_checked_true_activates_and_provenance(self):
        result, stats = self.audit("true")
        self.assertEqual(stats["trace"], "SCPCAC")
        self.assertEqual(result["check_before"]["value"], 0)
        self.assertEqual(result["activate"]["called"], 1)
        self.assertEqual(result["check_final"]["value"], 1)
        self.assertEqual(result["audit_complete"], 1)
        for symbol in result["symbols"].values():
            self.assertEqual(symbol["available"], 1)
            self.assertEqual(symbol["dladdr_ok"], 1)
            self.assertTrue(symbol["library"].endswith(".so"))
            self.assertTrue(symbol["symbol"].startswith("OH_"))

    def test_before_true_but_after_false_does_not_activate(self):
        result, stats = self.audit("after_false")
        self.assertEqual(result["check_before"]["value"], 1)
        self.assertEqual(result["check_after"]["value"], 0)
        self.assertNotIn("A", stats["trace"])

    def test_failed_or_malformed_check_cannot_authorize_activation(self):
        for mode in ("error_check", "bad_count", "null_pointer"):
            with self.subTest(mode=mode):
                result, stats = self.audit(mode)
                self.assertEqual(result["activation_eligible"], 0)
                self.assertEqual(result["check_after"]["value"], -1)
                self.assertNotIn("A", stats["trace"])
                self.assertEqual(stats["freed"], 0 if mode == "null_pointer" else 3)

    def test_public_release_preserves_numeric_policy_errors(self):
        result, stats = self.audit("policy_errors")
        self.assertEqual(stats["trace"], "SCPRCARC")
        self.assertEqual(stats["released"], 2)
        for name, code in (("persist", 1), ("activate", 4)):
            self.assertEqual(result[name]["rc"], 13900001)
            self.assertEqual(result[name]["error_codes"], [code])
            self.assertEqual(result[name]["released"], 1)

    def test_oversized_error_array_is_not_inspected_but_is_released(self):
        result, stats = self.audit("big_error_count")
        for name in ("persist", "activate"):
            self.assertEqual(result[name]["result_count"], 17)
            self.assertEqual(result[name]["codes_truncated"], 1)
            self.assertEqual(result[name]["error_codes"], [])
            self.assertEqual(result[name]["shape_ok"], 0)
        self.assertEqual(stats["released"], 2)

    def test_missing_fileshare_library_is_graceful(self):
        result, stats = self.audit(variant="missing_library")
        self.assertEqual(result["libraries"]["fileshare_loaded"], 0)
        self.assertEqual(result["audit_complete"], 0)
        self.assertEqual(result["persist"]["called"], 0)
        self.assertEqual(stats["trace"], "S")

    def test_missing_required_symbol_skips_sequence(self):
        for variant in ("OMIT_CHECK", "OMIT_PERSIST", "OMIT_ACTIVATE", "OMIT_RELEASE"):
            with self.subTest(variant=variant):
                result, stats = self.audit(variant=variant)
                self.assertEqual(result["libraries"]["fileshare_loaded"], 1)
                self.assertEqual(result["audit_complete"], 0)
                self.assertEqual(stats["trace"], "S")
                self.assertEqual(result["check_before"]["called"], 0)

    def test_missing_access_library_is_reported_without_faking_permission(self):
        result, stats = self.audit(variant="missing_access")
        self.assertEqual(result["self_permission"], {"called": 0, "granted": -1})
        self.assertEqual(stats["trace"], "CPCC")

    def test_invalid_uri_never_loads_or_calls_policy_libraries(self):
        for uri in ("", "file://", "https://example.invalid", "file:///x\n", "file:///x\x7f", "file:///x\0", "file:///" + "x" * 8192):
            with self.subTest(uri=repr(uri[:40])):
                result, stats = self.audit(uri=uri)
                self.assertEqual(result["uri_valid"], 0)
                self.assertEqual(result["input_error"], 401)
                self.assertEqual(stats["trace"], "")
                self.assertNotIn("libraries", result)

    def test_maximum_uri_is_valid_and_not_echoed(self):
        uri = "file:///" + "x" * (8192 - 8)
        result, stats = self.audit(uri=uri)
        self.assertEqual(result["uri_valid"], 1)
        self.assertEqual(result["uri_length"], 8192)
        self.assertEqual(stats["trace"], "SCPCC")
        self.assertNotIn(uri, json.dumps(result))


if __name__ == "__main__":
    unittest.main(verbosity=2)
