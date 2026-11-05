#pragma once
#include "entry_support.h"
#include <arpa/inet.h>
#include <array>
#include <netinet/in.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <thread>

namespace pico::services::entry {
// Only the current child signals ITSELF when the private registry cancels its
// intent. HAP crash cleanup remains the official child API/system mechanism.
class IntentMonitor {
    std::atomic<bool> finished_{false};
    std::thread thread_;
    static bool Transfer(int fd,void *data,size_t size,bool sending){
        auto *p=static_cast<unsigned char *>(data);size_t done=0;
        while(done<size){ssize_t n=sending?send(fd,p+done,size-done,MSG_NOSIGNAL):recv(fd,p+done,size-done,0);
            if(n<0&&errno==EINTR)continue;if(n<=0)return false;done+=static_cast<size_t>(n);}
        return true;
    }
    static bool Cancelled(const json &b){
        auto address=b.at("control_address").get<std::string>();
        auto colon=address.find(':');
        if(colon==std::string::npos||address.substr(0,colon)!="127.0.0.1")return false;
        int port=std::stoi(address.substr(colon+1));if(port<1||port>65535)return false;
        const auto capability=b.at("control_capability").get<std::string>();if(capability.size()!=64)return false;
        std::array<unsigned char,60> request{};memcpy(request.data(),"PNS1",4);request[4]=2;
        uint64_t id=b.at("instance").get<uint64_t>();
        for(size_t n=0;n<8;++n){request[15-n]=static_cast<unsigned char>(id>>(8*n));request[23-n]=request[15-n];}
        for(size_t n=0;n<32;++n){auto text=capability.substr(2*n,2);size_t consumed=0;
            auto byte=std::stoul(text,&consumed,16);if(consumed!=2||byte>255)return false;request[28+n]=static_cast<unsigned char>(byte);}
        int fd=socket(AF_INET,SOCK_STREAM|SOCK_CLOEXEC,0);if(fd<0)return false;
        timeval timeout{1,0};setsockopt(fd,SOL_SOCKET,SO_RCVTIMEO,&timeout,sizeof(timeout));setsockopt(fd,SOL_SOCKET,SO_SNDTIMEO,&timeout,sizeof(timeout));
        sockaddr_in peer{};peer.sin_family=AF_INET;peer.sin_port=htons(port);peer.sin_addr.s_addr=htonl(INADDR_LOOPBACK);
        std::array<uint32_t,3> response{};
        bool ok=connect(fd,reinterpret_cast<sockaddr *>(&peer),sizeof(peer))==0&&Transfer(fd,request.data(),request.size(),true)&&Transfer(fd,response.data(),sizeof(response),false);
        uint32_t size=ntohl(response[2]);
        if(!ok||ntohl(response[0])!=0x504e5231||ntohl(response[1])||size>65536){close(fd);return false;}
        std::string body(size,'\0');ok=Transfer(fd,body.data(),body.size(),false);close(fd);if(!ok)return false;
        auto status=json::parse(body);auto record=status.at("record");
        return record.at("hap_session")==b.at("hap_session")&&record.at("instance")==b.at("instance")&&record.at("pid")==getpid()&&record.at("official_created")==true&&record.at("stop_requested")==true;
    }
public:
    explicit IntentMonitor(json bootstrap):thread_([this,bootstrap=std::move(bootstrap)](){
        while(!finished_){try{if(Cancelled(bootstrap)){kill(getpid(),SIGTERM);return;}}catch(...){/* Unknown is not parent death. */}
            for(int n=0;n<10&&!finished_;++n)usleep(10000);}
    }){}
    ~IntentMonitor(){finished_=true;if(thread_.joinable())thread_.join();}
};
} // namespace pico::services::entry
