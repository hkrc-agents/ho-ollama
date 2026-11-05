// Trusted official-child SO wrapper. Load one Go runtime per dedicated child;
// never dlclose it, reuse its entry, fork, or exec another service.
#include "entry_support.h"
#include <dlfcn.h>

extern "C" __attribute__((visibility("default"))) void Main(NativeChildProcess_Args args){
    int code=125;
    try{
        pico::services::entry::Invocation invocation(args);
        const auto kind=invocation.launch.at("kind").get<std::string>();
        if(kind=="storage-probe"){
            // Official siblings cannot inherit the Go APP's descriptor via a
            // TCP request. Reopen its exclusively reserved probe directory and
            // verify dev/inode before passing it to the C SO probe. Do not
            // overwrite any appspawn/AbilityKit internal descriptor (e.g. 3).
            auto path=invocation.launch.at("probe_directory").get<std::string>();
            if(path.empty()||path.front()!='/'||path.find('\0')!=std::string::npos)throw std::runtime_error("invalid storage probe path");
            int fd=open(path.c_str(),O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC);struct stat info{};
            if(fd<0||fstat(fd,&info)||static_cast<uint64_t>(info.st_dev)!=invocation.launch.at("probe_dev").get<uint64_t>()||static_cast<uint64_t>(info.st_ino)!=invocation.launch.at("probe_ino").get<uint64_t>()){
                if(fd>=0)close(fd);throw std::runtime_error("storage probe directory identity mismatch");
            }
            void *handle=dlopen("libpico_storage_probe.so",RTLD_NOW|RTLD_LOCAL);
            if(!handle)throw std::runtime_error("storage probe SO unavailable");
            using Probe=int(*)(int,char **);auto probe=reinterpret_cast<Probe>(dlsym(handle,"PicoStorageProbeMain"));
            using DirectoryFd=void(*)(int);auto setDirectory=reinterpret_cast<DirectoryFd>(dlsym(handle,"PicoStorageProbeSetDirectoryFd"));
            if(!probe||!setDirectory)throw std::runtime_error("storage probe entry unavailable");
            setDirectory(fd);
            char name[]="storage-probe";char *argv[]={name,nullptr};code=probe(1,argv);close(fd);invocation.Returned(code);fflush(nullptr);_exit(code);
        }
        const char *library=kind=="core"?"libpico_ollama_core.so":
            (kind=="app"||kind=="model-store")?"libpico_ollama_app.so":nullptr;
        if(!library)throw std::runtime_error("unsupported Go component kind");
        // dlopen bare names are fixed by trusted packaging, never request data.
        void *handle=dlopen(library,RTLD_NOW|RTLD_LOCAL);
        if(!handle)throw std::runtime_error("managed Go runtime could not be loaded");
        using Run=int(*)(const char *);
        auto run=reinterpret_cast<Run>(dlsym(handle,"PicoServiceRun"));
        if(!run)throw std::runtime_error("managed Go service entry missing");
        code=run(invocation.text.c_str());
        invocation.Returned(code);
    }catch(const std::exception &error){fprintf(stderr,"managed Go SO failed: %s\n",error.what());}
    fflush(nullptr);
    _exit(code);
}
