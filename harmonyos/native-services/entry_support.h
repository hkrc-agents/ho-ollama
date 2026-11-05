#pragma once
#ifndef __OHOS__
#error "Official service entries are HarmonyOS-only"
#endif

#include <AbilityKit/native_child_process.h>
#include <nlohmann/json.hpp>
#include <algorithm>
#include <atomic>
#include <cerrno>
#include <csignal>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <map>
#include <stdexcept>
#include <string>
#include <sys/resource.h>
#include <sys/stat.h>
#include <unistd.h>

namespace pico::services::entry {
using json = nlohmann::json;

inline int Exclusive(int directory, const char *name) {
    int fd=openat(directory,name,O_CREAT|O_EXCL|O_WRONLY|O_NOFOLLOW|O_CLOEXEC,0600);
    if(fd<0)throw std::runtime_error("cannot exclusively create managed output");
    if(fd<=2){int moved=fcntl(fd,F_DUPFD_CLOEXEC,3);close(fd);fd=moved;}
    if(fd<0)throw std::runtime_error("cannot reserve managed output descriptor");
    return fd;
}
inline void WriteAll(int fd,const std::string &text){
    size_t done=0;while(done<text.size()){
        ssize_t n=write(fd,text.data()+done,text.size()-done);
        if(n<0&&errno==EINTR)continue;
        if(n<=0)throw std::runtime_error("managed receipt write failed");done+=static_cast<size_t>(n);
    }
    if(fsync(fd))throw std::runtime_error("managed receipt sync failed");
}
struct Invocation {
    json bootstrap, launch;
    std::string text;
    int directory=-1;
    explicit Invocation(NativeChildProcess_Args args){
        if(!args.entryParams||strnlen(args.entryParams,65537)>65536)throw std::runtime_error("invalid bootstrap size");
        bootstrap=json::parse(args.entryParams);
        if(bootstrap.at("schema")!=1||!bootstrap.at("instance").is_number_unsigned())throw std::runtime_error("invalid bootstrap identity");
        launch=json::parse(bootstrap.at("payload").get<std::string>());
        if(launch.at("schema")!=1||!launch.at("argv").is_array()||launch.at("argv").empty()||launch.at("argv").size()>256||
           !launch.at("env").is_array()||launch.at("env").size()>256)throw std::runtime_error("invalid launch schema");
        const auto kind=launch.at("kind").get<std::string>(),role=bootstrap.at("role").get<std::string>();
        const std::string expected=(kind=="model-store"||kind=="storage-probe")?"worker":kind;
        if(expected!=role||(kind!="app"&&kind!="core"&&kind!="runner"&&kind!="standalone"&&kind!="model-store"&&kind!="storage-probe"))throw std::runtime_error("invalid role/kind");
        auto path=launch.at("directory").get<std::string>();
        if(path.empty()||path.front()!='/'||path=="/"||path.find('\0')!=std::string::npos)throw std::runtime_error("invalid managed directory");
        directory=open(path.c_str(),O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);
        struct stat st{};
        if(directory<0||fstat(directory,&st)||st.st_uid!=getuid()||(st.st_mode&0077))throw std::runtime_error("managed directory is not private");
        // Legacy memory_mib is compatibility metadata, never a virtual-address cap.
        for(const char *stream:{"stdout","stderr"}){
            int fd=Exclusive(directory,stream);
            int target=std::strcmp(stream,"stdout")==0?STDOUT_FILENO:STDERR_FILENO;
            if(dup2(fd,target)<0){close(fd);throw std::runtime_error("managed stream redirection failed");}close(fd);
        }
        int input=open("/dev/null",O_RDONLY|O_CLOEXEC);
        if(input<0||dup2(input,STDIN_FILENO)<0)throw std::runtime_error("managed stdin redirection failed");
        if(input>2)close(input);
        // Environment keys come only from the authenticated private control
        // plane. Reject duplicate/NUL inputs rather than truncating at libc.
        std::map<std::string,std::string> env;
        for(const auto &v:launch.at("env")){
            auto item=v.get<std::string>();auto pos=item.find('=');
            if(pos==std::string::npos||pos==0||item.find('\0')!=std::string::npos||!env.emplace(item.substr(0,pos),item.substr(pos+1)).second)throw std::runtime_error("invalid managed environment");
        }
        if(clearenv())throw std::runtime_error("managed clearenv failed");
        for(const auto &[key,value]:env)if(setenv(key.c_str(),value.c_str(),1))throw std::runtime_error("managed environment failed");
        auto proof=json{{"hap_session",bootstrap.at("hap_session")},{"instance",bootstrap.at("instance")},{"pid",getpid()},
                        {"uid",getuid()},{"address_space_policy","not-set"}};
        int fd=Exclusive(directory,"entered.json");WriteAll(fd,proof.dump()+"\n");close(fd);
        text=bootstrap.dump();
    }
    ~Invocation(){if(directory>=0)close(directory);}
    void Returned(int code){
        fflush(nullptr);
        auto receipt=json{{"hap_session",bootstrap.at("hap_session")},{"instance",bootstrap.at("instance")},{"pid",getpid()},{"code",code}};
        int fd=Exclusive(directory,"return.pending");WriteAll(fd,receipt.dump()+"\n");close(fd);
        if(renameat(directory,"return.pending",directory,"return.json")||fsync(directory))throw std::runtime_error("managed return publication failed");
    }
};
} // namespace pico::services::entry
