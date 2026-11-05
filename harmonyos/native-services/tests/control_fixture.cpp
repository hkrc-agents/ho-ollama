// Host-only protocol interoperability fixture. No OS child service is spawned.
#include "registry.h"
#include "control_server.h"
#include <iostream>
#include <map>
#include <memory>
#include <mutex>
#include <string>

using namespace pico::services;
namespace {
class ModelPlatform final : public Platform {
public:
    std::mutex mutex;
    Exit handler;
    int32_t next = 100;
    std::map<int32_t,uint64_t> live;
    bool SetExitHandler(Exit value) override {
        std::lock_guard<std::mutex> lock(mutex);handler=std::move(value);return true;
    }
    Started Start(const Record &) override {
        std::lock_guard<std::mutex> lock(mutex);int32_t pid=next++;live[pid]=static_cast<uint64_t>(pid)*10;
        return {0,pid,static_cast<uint64_t>(pid)*10};
    }
    bool Matches(int32_t pid,uint64_t ticks) override {
        std::lock_guard<std::mutex> lock(mutex);auto found=live.find(pid);return found!=live.end()&&found->second==ticks;
    }
    int Kill(int32_t pid) override {Emit(pid,9);return 0;}
    void Emit(int32_t pid,int32_t signal) {
        Exit callback;
        {std::lock_guard<std::mutex> lock(mutex);live.erase(pid);callback=handler;}
        if(callback)callback(pid,signal);
    }
    bool Audit(const char *,const Record &) override{return true;}
};
}
int main() {
    auto platform=std::make_shared<ModelPlatform>();
    Registry registry(std::string(64,'b'),platform);
    auto app=registry.Start({Role::App,0,"host mock APP"});if(!app.ok)return 1;
    ControlServer control(registry,std::string(64,'a'));
    auto address=control.Start();
    std::cout<<"{\"control_address\":\""<<address<<"\",\"control_capability\":\""<<std::string(64,'a')
             <<"\",\"hap_session\":\""<<std::string(64,'b')<<"\",\"instance\":"<<app.instance<<"}"<<std::endl;
    std::string line;
    while(std::getline(std::cin,line)) {
        if(line=="quit")break;
        if(line.rfind("crash ",0)==0) {
            Instance id=std::stoull(line.substr(6));Record record;bool drained=false;
            if(registry.Inspect(id,record,drained)&&!drained)platform->Emit(record.pid,11);
        }
    }
    registry.AbortTree(app.instance);
    registry.Pump();
    control.Stop();
    return 0;
}
