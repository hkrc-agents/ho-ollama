"""Bounded host checks of the exact scoped HAP subreaper/drain implementation."""
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[1]
HEADER=ROOT/'hap/entry/src/main/cpp'
HARNESS=r'''
#include "elf_supervision.h"
int main() {
    alarm(8);
    if(!pico_elf::EnableSubreaper())return 10;
    std::vector<pid_t> initial;
    if(!pico_elf::DirectChildren(initial)||!initial.empty())return 11;
    int ready[2];if(pipe(ready))return 12;
    pid_t parent=fork();
    if(parent<0)return 13;
    if(parent==0){
        alarm(5);close(ready[0]);
        pid_t leaf=fork();if(leaf<0)_exit(20);
        if(leaf==0){
            alarm(5);
            // Even an escaped group is recaptured after APP death by the
            // host subreaper; never infer that it belonged to PID 1.
            if(setsid()<0)_exit(21);
            char b='r';if(write(ready[1],&b,1)!=1)_exit(22);
            for(;;)pause();
        }
        for(;;)pause();
    }
    close(ready[1]);char b=0;if(read(ready[0],&b,1)!=1||b!='r')return 14;
    close(ready[0]);
    if(kill(parent,SIGKILL))return 15;
    int status=0;if(waitpid(parent,&status,0)!=parent)return 16;
    if(!pico_elf::DrainAdopted(std::chrono::seconds(2)))return 17;
    std::vector<pid_t> children;
    if(!pico_elf::DirectChildren(children)||!children.empty())return 18;
    return 0;
}
'''


class ScopedDrain(unittest.TestCase):
    def test_proc_enumeration_error_is_not_an_empty_tree(self):
        harness = r'''
#include <dirent.h>
#include <cerrno>
static bool fail = true;
static dirent* failed_readdir(DIR*) { if (fail) errno = EIO; return nullptr; }
#define readdir failed_readdir
#include "elf_supervision.h"
#undef readdir
int main() {
    std::vector<pid_t> children;
    if (pico_elf::DirectChildren(children)) return 10;
    if (pico_elf::DrainAdopted(std::chrono::milliseconds(1))) return 11;
    fail = false;
    errno = EACCES; // Unrelated prior errors must not turn normal EOF into failure.
    if (!pico_elf::DirectChildren(children)) return 12;
    if (!pico_elf::DrainAdopted(std::chrono::milliseconds(1))) return 13;
    return 0;
}
'''
        with tempfile.TemporaryDirectory(prefix='pico-elf-proc-error-') as root:
            source=Path(root)/'test.cpp'; source.write_text(harness)
            binary=Path(root)/'test'
            subprocess.run(['c++','-std=c++17','-Wall','-Wextra','-Werror','-pthread','-I'+str(HEADER),str(source),'-o',str(binary)],check=True,timeout=30)
            subprocess.run([str(binary)],check=True,timeout=3)

    def test_multilevel_fanout_and_detached_leaves_reclaimed(self):
        # More than one adoption round is needed: killing the former APP's
        # direct children exposes further descendants. Exercise the exact
        # production drain, not a duplicated Python implementation.
        harness = r'''
#include "elf_supervision.h"
static void tree(int depth, int ready) {
    alarm(10);
    signal(SIGTERM, SIG_IGN);
    if (depth == 0) {
        if (setsid() < 0) _exit(21);
        char b = 'r';
        if (write(ready, &b, 1) != 1) _exit(22);
    } else {
        for (int i = 0; i < 2; ++i) {
            pid_t child = fork();
            if (child < 0) _exit(23);
            if (child == 0) tree(depth - 1, ready);
        }
    }
    for (;;) pause();
}
int main() {
    alarm(12);
    if (!pico_elf::EnableSubreaper()) return 10;
    int ready[2]; if (pipe(ready)) return 11;
    pid_t app = fork(); if (app < 0) return 12;
    if (app == 0) { close(ready[0]); tree(3, ready[1]); }
    close(ready[1]);
    int count = 0; char b;
    while (count < 8 && read(ready[0], &b, 1) == 1 && b == 'r') ++count;
    close(ready[0]);
    int error = count == 8 ? 0 : 13;
    if (kill(app, SIGKILL)) error = 14;
    int status = 0; if (waitpid(app, &status, 0) != app) error = 15;
    if (!pico_elf::DrainAdopted(std::chrono::seconds(3))) error = 16;
    std::vector<pid_t> left;
    if (!pico_elf::DirectChildren(left) || !left.empty()) error = 17;
    return error;
}
'''
        with tempfile.TemporaryDirectory(prefix='pico-elf-fanout-') as root:
            source=Path(root)/'test.cpp'; source.write_text(harness)
            binary=Path(root)/'test'
            subprocess.run(['c++','-std=c++17','-O2','-Wall','-Wextra','-Werror','-pthread','-I'+str(HEADER),str(source),'-o',str(binary)],check=True,timeout=30)
            for _ in range(3):
                subprocess.run([str(binary)],check=True,timeout=15)

    def test_explicit_run_intent_not_crash_retry_epoch(self):
        with tempfile.TemporaryDirectory(prefix='pico-elf-intent-') as root:
            source=Path(root)/'test.cpp'
            source.write_text('''#include "elf_supervision.h"
#include <cassert>
int main(){bool desired=false;uint64_t run=0,epoch=0;
pico_elf::StartIntent(desired,run,epoch);assert(desired&&run==1&&epoch==1);
pico_elf::StartIntent(desired,run,epoch);assert(desired&&run==1&&epoch==2);
pico_elf::StopIntent(desired,epoch);assert(!desired&&run==1&&epoch==3);
pico_elf::StartIntent(desired,run,epoch);assert(desired&&run==2&&epoch==4);
return 0;}\n''')
            binary=Path(root)/'test'
            subprocess.run(['c++','-std=c++17','-Wall','-Wextra','-Werror','-pthread','-I'+str(HEADER),str(source),'-o',str(binary)],check=True,timeout=30)
            subprocess.run([str(binary)],check=True,timeout=3)

    def test_adopted_tree_and_escaped_group_reclaimed(self):
        with tempfile.TemporaryDirectory(prefix='pico-elf-unit-') as root:
            source=Path(root)/'test.cpp';source.write_text(HARNESS)
            binary=Path(root)/'test'
            subprocess.run(['c++','-std=c++17','-O2','-Wall','-Wextra','-Werror','-pthread','-I'+str(HEADER),str(source),'-o',str(binary)],check=True,timeout=30)
            for _ in range(3):
                subprocess.run([str(binary)],check=True,timeout=10)

    def test_same_uid_peer_is_not_scoped_child(self):
        with tempfile.TemporaryDirectory(prefix='pico-elf-peer-') as root:
            source=Path(root)/'test.cpp'
            source.write_text('#include "elf_supervision.h"\nint main(){return pico_elf::EnableSubreaper()&&pico_elf::DrainAdopted()?0:1;}\n')
            binary=Path(root)/'test'
            subprocess.run(['c++','-std=c++17','-Wall','-Wextra','-Werror','-pthread','-I'+str(HEADER),str(source),'-o',str(binary)],check=True,timeout=30)
            peer=subprocess.Popen(['/bin/sleep','5'])
            try:
                subprocess.run([str(binary)],check=True,timeout=3)
                self.assertIsNone(peer.poll(),'same UID peer was killed')
            finally:
                peer.terminate();peer.wait(timeout=3)


if __name__=='__main__':unittest.main()
