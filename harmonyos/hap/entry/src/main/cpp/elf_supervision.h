#pragma once
// Private HarmonyOS host policy, not part of upstream inference engines.
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <dirent.h>
#include <signal.h>
#include <unistd.h>
#include <cerrno>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

namespace pico_elf {
inline void StartIntent(bool& desired,uint64_t& run,uint64_t& epoch) {
    if(!desired)++run;
    desired=true;++epoch;
}
inline void StopIntent(bool& desired,uint64_t& epoch) {
    desired=false;++epoch;
}
inline bool EnableSubreaper() {
    int value=0;
    return !prctl(PR_SET_CHILD_SUBREAPER,1) && !prctl(PR_GET_CHILD_SUBREAPER,&value) && value==1;
}

// Only current, real, direct children. Not PPID=1, UID peers or disk PID records.
// A live/zombie direct child's PID remains reserved until this host reaps it.
inline bool DirectChildren(std::vector<pid_t>& children) {
    DIR* dir=opendir("/proc");if(!dir)return false;
    bool ok=true;struct dirent* entry;
    for(;;){
        // readdir returns nullptr both at EOF and on error. Reset errno for
        // this call; stat/fopen races below must not masquerade as its error.
        errno=0;entry=readdir(dir);
        if(!entry){if(errno)ok=false;break;}
        char* end=nullptr;long pid=strtol(entry->d_name,&end,10);
        if(!*entry->d_name||*end||pid<=1)continue;
        std::string path="/proc/"+std::to_string(pid);
        struct stat st{};
        if(stat(path.c_str(),&st)){if(errno!=ENOENT&&errno!=ESRCH)ok=false;continue;}
        if(st.st_uid!=getuid())continue;
        FILE* file=fopen((path+"/stat").c_str(),"r");
        if(!file){if(errno!=ENOENT&&errno!=ESRCH)ok=false;continue;}
        char data[4096]={};size_t n=fread(data,1,sizeof(data)-1,file);
        bool bad=ferror(file);fclose(file);
        char* tail=n?strrchr(data,')'):nullptr;char state=0;long parent=0;
        if(bad||!tail||sscanf(tail+1," %c %ld",&state,&parent)!=2){ok=false;continue;}
        if(parent==getpid())children.push_back(static_cast<pid_t>(pid));
    }
    if(closedir(dir))ok=false;
    return ok;
}

// Caller serializes every host spawn/reap and permits only APP as a direct
// product child. Called AFTER that APP has exited, never while it is live.
inline bool DrainAdopted(std::chrono::milliseconds budget=std::chrono::seconds(10)) {
    const auto end=std::chrono::steady_clock::now()+budget;
    for(;;){
        std::vector<pid_t> children;
        if(!DirectChildren(children))return false;
        if(children.empty())return true;
        for(pid_t pid:children){
            if(kill(pid,SIGKILL)&&errno!=ESRCH)return false;
            int status=0;pid_t result;
            do{result=waitpid(pid,&status,WNOHANG);}while(result<0&&errno==EINTR);
            if(result<0&&errno!=ECHILD&&errno!=ESRCH)return false;
        }
        if(std::chrono::steady_clock::now()>=end)return false;
        std::this_thread::sleep_for(std::chrono::milliseconds(20));
    }
}
} // namespace pico_elf
