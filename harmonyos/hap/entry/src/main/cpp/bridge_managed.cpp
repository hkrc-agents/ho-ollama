// Explicit official-SO build. No spawn/fork/exec, waitpid, old-PID adoption,
// group signalling or ArkTS creation proxy exists in this implementation.
#ifndef __OHOS__
#error "Managed Ollama HAP bridge is HarmonyOS-only"
#endif
#include <napi/native_api.h>
#include <nlohmann/json.hpp>
#include <dlfcn.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <atomic>
#include <fstream>
#include <iterator>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>
#include "registry.h"
#include "ohos_platform.h"
#include "supervisor.h"
#include "control_server.h"
#include "storage_permission_audit.h"

namespace {
using nlohmann::json;
using namespace pico::services;
std::mutex mutex;
std::string String(napi_env env,napi_value value){
    size_t size=0;if(napi_get_value_string_utf8(env,value,nullptr,0,&size)!=napi_ok||size>8192)return {};
    std::vector<char> bytes(size+1);if(napi_get_value_string_utf8(env,value,bytes.data(),bytes.size(),&size)!=napi_ok)return {};
    return std::string(bytes.data(),size);
}
napi_value Text(napi_env env,const std::string &text){napi_value value;napi_create_string_utf8(env,text.data(),text.size(),&value);return value;}
bool ValidRoot(const std::string &root){return root.rfind("/data/storage/",0)==0&&root.find("..") == std::string::npos&&root.find('\0')==std::string::npos;}
std::string Parent(const std::string &path){auto split=path.rfind('/');if(split==std::string::npos)throw std::runtime_error("invalid installed library path");return path.substr(0,split);}
void Directory(const std::string &path){if(mkdir(path.c_str(),0700)&&errno!=EEXIST)throw std::runtime_error("private runtime directory unavailable");struct stat st{};if(lstat(path.c_str(),&st)||!S_ISDIR(st.st_mode)||st.st_uid!=getuid())throw std::runtime_error("unsafe private runtime directory");}

class Runtime {
public:
    const std::string root;
    std::string session,directory,address,capability,libraryRoot;
    std::shared_ptr<Platform> platform;
    std::unique_ptr<Registry> registry;
    std::unique_ptr<ControlServer> control;
    std::unique_ptr<AppSupervisor> supervisor;
    std::atomic<bool> finished{false};
    std::thread monitor;
    Runtime(std::string home):root(std::move(home)){
        Dl_info info{};if(!dladdr(reinterpret_cast<void *>(&Parent),&info)||!info.dli_fname)throw std::runtime_error("installed bridge identity unavailable");libraryRoot=Parent(info.dli_fname);
        session=NewSessionID();capability=NewSessionID();if(session==capability)throw std::runtime_error("independent capability generation failed");
        Directory(root+"/managed-services");directory=root+"/managed-services/"+session;Directory(directory);Directory(root+"/desktop");Directory(root+"/models");Directory(root+"/.ollama");Directory(root+"/.ollama/logs");Directory(root+"/tmp");
        auto entries=std::map<Role,std::string>{{Role::App,"libpico_go_child.so:Main"},{Role::Core,"libpico_go_child.so:Main"},
            {Role::Runner,"libpico_llama_server.so:Main"},{Role::Standalone,"libpico_llama_server.so:Main"},{Role::Worker,"libpico_go_child.so:Main"}};
        platform=CreateOHOSPlatform(directory+"/lifecycle.jsonl",entries,[this](const Record &record){
            auto boot=json::parse(Bootstrap(record));boot["control_address"]=address;boot["control_capability"]=capability;
            // APP replacement gets fresh output/receipt files. Child requests
            // already reserve unique directories in the caller's private tree.
            if(record.spec.role==Role::App){
                auto payload=json::parse(record.spec.payload);auto call=directory+"/app-"+std::to_string(record.instance);Directory(call);payload["directory"]=call;boot["payload"]=payload.dump();
                unlink((root+"/desktop/app-token").c_str());
                if(unlink((root+"/desktop/shutdown-complete").c_str())&&errno!=ENOENT)throw std::runtime_error("stale shutdown receipt could not be removed");
            }
            return boot.dump();
        });
        registry=std::make_unique<Registry>(session,platform);control=std::make_unique<ControlServer>(*registry,capability);address=control->Start();
        supervisor=std::make_unique<AppSupervisor>(*registry);
        monitor=std::thread([this](){while(!finished){supervisor->Tick();usleep(25000);}});
    }
    ~Runtime(){finished=true;if(monitor.joinable())monitor.join();if(control)control->Stop();}
    Outcome Start(int port){
#ifdef PICO_MANAGED_PROBE_ONLY
        const int appPort=port+1; // Isolated test ports; never collide with the product.
#else
        const int appPort=port==11435?11437:11435;
#endif
        constexpr const char *bin="/data/service/hnp/ollama.org/ollama_1.0/bin";
        std::vector<std::string> env={"HOME="+root,"TMPDIR="+root+"/tmp","PATH="+std::string(bin)+":/system/bin",
            "PICO_NATIVE_LIB_DIR="+libraryRoot,"PICO_COMPONENT_BIN_DIR="+std::string(bin),
            "OLLAMA_HOST=127.0.0.1:"+std::to_string(port),"OLLAMA_MODELS="+root+"/models",
            "OLLAMA_NUM_PARALLEL=1","OLLAMA_CONTEXT_LENGTH=1024","OLLAMA_MAX_LOADED_MODELS=1","OLLAMA_NO_CLOUD=1"};
#ifdef PICO_MANAGED_PROBE_ONLY
        env.push_back("PICO_NATIVE_SELF_TEST=1");
#endif
        auto payload=json{{"schema",1},{"kind","app"},{"argv",json::array({"ollama-app","--listen","127.0.0.1:"+std::to_string(appPort),"--data",root+"/desktop"})},
            {"env",env},{"directory",directory+"/reserved-by-native"}};
        return supervisor->Start(payload.dump());
    }
    bool Inspect(Record &record,bool &quiet){auto id=supervisor->Snapshot().current;bool drained=false;return id&&registry->Inspect(id,record,drained,&quiet);}
};
std::unique_ptr<Runtime> runtime;

std::string RootArg(napi_env env,napi_callback_info info){size_t n=1;napi_value value;napi_get_cb_info(env,info,&n,&value,nullptr,nullptr);return n?String(env,value):"";}
bool Same(const std::string &root){return runtime&&runtime->root==root;}
napi_value AppStart(napi_env env,napi_callback_info info){
    size_t count=2;napi_value args[2];napi_get_cb_info(env,info,&count,args,nullptr,nullptr);int32_t port=0;
    if(count!=2||napi_get_value_int32(env,args[1],&port)!=napi_ok)return Text(env,"error: app arguments");
    auto root=String(env,args[0]);if(!ValidRoot(root)||port<1024||port>65535)return Text(env,"error: directory/port");
    std::lock_guard<std::mutex> lock(mutex);
    try{
        if(runtime&&!Same(root))return Text(env,"error: another private root owns this HAP session");
        if(!runtime)runtime=std::make_unique<Runtime>(root);
        Record record;bool quiet=false;
        if(runtime->Inspect(record,quiet)&&!quiet){if(record.state==State::Running&&record.spec.desired)return Text(env,"running pid="+std::to_string(record.pid));return Text(env,"error: previous APP tree not quiescent");}
        auto result=runtime->Start(port);if(!result.ok)return Text(env,"error: "+result.error);
        if(!runtime->Inspect(record,quiet))return Text(env,"error: missing native APP registration");
        return Text(env,"starting pid="+std::to_string(record.pid));
    }catch(const std::exception &error){return Text(env,"error: "+std::string(error.what()));}
}
napi_value AppStop(napi_env env,napi_callback_info info){
    auto root=RootArg(env,info);std::lock_guard<std::mutex> lock(mutex);
    if(!ValidRoot(root)||!Same(root))return Text(env,"error: no matching Native session");
    runtime->supervisor->RequestQuit();return Text(env,"stopping");
}
napi_value AppToken(napi_env env,napi_callback_info info){
    auto root=RootArg(env,info);std::lock_guard<std::mutex> lock(mutex);Record record;bool quiet=false;
    if(!Same(root)||!runtime->Inspect(record,quiet)||quiet||record.state!=State::Running||!record.spec.desired)return Text(env,"");
    std::ifstream input(root+"/desktop/app-token");std::string token;input>>token;
    if(token.size()!=64||token.find_first_not_of("0123456789abcdef")!=std::string::npos)return Text(env,"");return Text(env,token);
}
napi_value AppShutdownStatus(napi_env env,napi_callback_info info){
    auto root=RootArg(env,info);std::lock_guard<std::mutex> lock(mutex);Record record;bool quiet=false;
    if(!Same(root)||!runtime->Inspect(record,quiet))return Text(env,"error: missing Native shutdown instance");
    if(!quiet)return Text(env,"stopping");
    std::ifstream receipt(root+"/desktop/shutdown-complete");int64_t pid=-1;uint64_t ticks=0;std::string extra;
    if(!(receipt>>pid>>ticks)||(receipt>>extra)||pid!=record.pid||ticks!=record.startTicks)return Text(env,"error: APP exited without matching service shutdown acknowledgement");
    return Text(env,"stopped");
}
napi_value Status(napi_env env,napi_callback_info info){
    auto root=RootArg(env,info);std::lock_guard<std::mutex> lock(mutex);if(!ValidRoot(root))return Text(env,"error: directory");
    if(!Same(root))return Text(env,"stopped");
    for(const auto &record:runtime->registry->Snapshot())if(record.spec.role==Role::Core&&!record.exitObserved)return Text(env,"running pid="+std::to_string(record.pid));
    return Text(env,"stopped");
}
napi_value Stop(napi_env env,napi_callback_info info){auto root=RootArg(env,info);if(!ValidRoot(root))return Text(env,"error: directory");return Text(env,"stopped");} // APP owns core's graceful stop/ACK.
napi_value Logs(napi_env env,napi_callback_info info){auto root=RootArg(env,info);if(!ValidRoot(root))return Text(env,"error: directory");std::ifstream input(root+"/.ollama/logs/server.log",std::ios::binary);if(!input)return Text(env,"");input.seekg(0,std::ios::end);std::streamoff size=input.tellg();input.seekg(size>16384?size-std::streamoff(16384):std::streamoff(0));return Text(env,std::string(std::istreambuf_iterator<char>(input),{}));}
napi_value Unavailable(napi_env env,napi_callback_info){return Text(env,"error: executable debug launcher is disabled in official SO mode; use public HNP CLI");}
napi_value StoragePermissionAudit(napi_env env,napi_callback_info info){return Text(env,AuditStoragePermission(RootArg(env,info)));}
napi_value Init(napi_env env,napi_value exports){
    napi_property_descriptor props[]={
        {"start",nullptr,Unavailable,nullptr,nullptr,nullptr,napi_default,nullptr},{"stop",nullptr,Stop,nullptr,nullptr,nullptr,napi_default,nullptr},
        {"status",nullptr,Status,nullptr,nullptr,nullptr,napi_default,nullptr},{"logs",nullptr,Logs,nullptr,nullptr,nullptr,napi_default,nullptr},
        {"command",nullptr,Unavailable,nullptr,nullptr,nullptr,napi_default,nullptr},{"appStart",nullptr,AppStart,nullptr,nullptr,nullptr,napi_default,nullptr},
        {"appStop",nullptr,AppStop,nullptr,nullptr,nullptr,napi_default,nullptr},{"appToken",nullptr,AppToken,nullptr,nullptr,nullptr,napi_default,nullptr},
        {"appShutdownStatus",nullptr,AppShutdownStatus,nullptr,nullptr,nullptr,napi_default,nullptr},{"storagePermissionAudit",nullptr,StoragePermissionAudit,nullptr,nullptr,nullptr,napi_default,nullptr}};
    napi_define_properties(env,exports,sizeof(props)/sizeof(props[0]),props);return exports;
}
}
#ifndef PICO_MANAGED_PROBE_ONLY
static napi_module module={1,0,nullptr,Init,"ollamabridge",nullptr,{0}};
extern "C" __attribute__((constructor)) void Register(){napi_module_register(&module);}
#endif
