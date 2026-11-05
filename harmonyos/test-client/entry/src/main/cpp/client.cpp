#include <napi/native_api.h>
#include <atomic>
#include <algorithm>
#include <chrono>
#include <cerrno>
#include <cstdlib>
#include <fcntl.h>
#include <fstream>
#include <mutex>
#include <poll.h>
#include <signal.h>
#include <spawn.h>
#include <string>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>
#include <vector>

extern char **environ;
namespace {
constexpr size_t cap=262144;
std::mutex mutex;
std::string active;
std::atomic<bool> cancelled{false};
std::string pendingInput, liveOutput, liveError;
bool requestedEOF=false, stdinClosed=true;
size_t bufferedInput=0;
using Clock=std::chrono::steady_clock;
std::string str(napi_env env,napi_value v,size_t max=65536) {
    size_t n=0;if(napi_get_value_string_utf8(env,v,nullptr,0,&n)!=napi_ok||n>max)return {};
    std::vector<char> b(n+1);napi_get_value_string_utf8(env,v,b.data(),b.size(),&n);return {b.data(),n};
}
napi_value reply(napi_env e,const std::string &s){napi_value v;napi_create_string_utf8(e,s.data(),s.size(),&v);return v;}
bool id_ok(const std::string &s){if(s.empty()||s.size()>64)return false;for(unsigned char c:s)if(!((c>='a'&&c<='z')||(c>='A'&&c<='Z')||(c>='0'&&c<='9')||c=='_'||c=='-'))return false;return true;}
std::string quote(const std::string &s){std::string r="\"";const char h[]="0123456789abcdef";for(unsigned char c:s){if(c=='"'||c=='\\'){r+='\\';r+=c;}else if(c<32){r+="\\u00";r+=h[c>>4];r+=h[c&15];}else r+=c;}return r+'"';}
std::string b64(const std::string &s){static const char t[]="ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";std::string r;unsigned x=0;int n=0;for(unsigned char c:s){x=(x<<8)|c;n+=8;while(n>=6){n-=6;r+=t[(x>>n)&63];}}if(n)r+=t[(x<<(6-n))&63];while(r.size()%4)r+='=';return r;}
std::string read_small(const std::string &path){std::ifstream f(path);std::string s((std::istreambuf_iterator<char>(f)),{});if(s.size()>8192)s.resize(8192);return s;}
struct Job {std::string root,id,input,path;std::vector<std::string> args;int timeout=30000;bool interactive=false;};
void work(Job j) {
    // Suppress SIGPIPE for this worker only; never change the application's signal policy.
    sigset_t mask;sigemptyset(&mask);sigaddset(&mask,SIGPIPE);pthread_sigmask(SIG_BLOCK,&mask,nullptr);
    const auto begin=Clock::now();int input[2]={-1,-1},out[2]={-1,-1},err[2]={-1,-1};
    int rc=0,status=0;pid_t pid=-1,waited=0;bool timed=false,wasCancelled=false,limited=false,outEOF=false,errEOF=false;
    std::string output,error,childDomain,childStatus;
    auto closefd=[](int &fd){if(fd>=0){close(fd);fd=-1;}};
    if(pipe2(input,O_CLOEXEC)||pipe2(out,O_CLOEXEC)||pipe2(err,O_CLOEXEC))rc=errno;
    if(!rc)for(int fd:{input[1],out[0],err[0]})if(fcntl(fd,F_SETFL,fcntl(fd,F_GETFL)|O_NONBLOCK)<0){rc=errno;break;}
    if(!rc){
        posix_spawn_file_actions_t a;posix_spawnattr_t attr;
        rc=posix_spawn_file_actions_init(&a);
        if(!rc){
            rc=posix_spawnattr_init(&attr);
            if(!rc){
                rc=posix_spawnattr_setflags(&attr,POSIX_SPAWN_SETPGROUP);
                if(!rc)rc=posix_spawnattr_setpgroup(&attr,0);
                for(auto p:std::vector<std::pair<int,int>>{{input[0],0},{out[1],1},{err[1],2}})if(!rc)rc=posix_spawn_file_actions_adddup2(&a,p.first,p.second);
                for(int fd:{input[0],input[1],out[0],out[1],err[0],err[1]})if(!rc)rc=posix_spawn_file_actions_addclose(&a,fd);
                std::vector<std::string> vars;
                for(char **p=environ;*p;p++){std::string s=*p;if(s.rfind("HOME=",0)&&s.rfind("OLLAMA_HOST=",0)&&s.rfind("LD_LIBRARY_PATH=",0))vars.push_back(s);}
                vars.push_back("HOME="+j.root);vars.push_back("OLLAMA_HOST=127.0.0.1:11434");
                vars.push_back("LD_LIBRARY_PATH=/data/service/hnp/ollama.org/ollama_1.0/lib");
                std::vector<char*> env;for(auto &s:vars)env.push_back(s.data());env.push_back(nullptr);
                std::vector<std::string> av={"/data/service/hnp/pico_client_guard.org/pico_client_guard_1.0/bin/pico-client-guard","1536",j.path};
                for(size_t i=1;i<j.args.size();i++)av.push_back(j.args[i]);
                std::vector<char*> argv;for(auto &s:av)argv.push_back(s.data());argv.push_back(nullptr);
                if(!rc)rc=posix_spawn(&pid,av[0].c_str(),&a,&attr,argv.data(),env.data());
                posix_spawnattr_destroy(&attr);
            }
            posix_spawn_file_actions_destroy(&a);
        }
    }
    closefd(input[0]);closefd(out[1]);closefd(err[1]);
    if(!rc){
        childDomain=read_small("/proc/"+std::to_string(pid)+"/attr/current");
        childStatus=read_small("/proc/"+std::to_string(pid)+"/status");
        size_t sent=0;bool signalled=false;auto stopAt=begin;
        std::string queued=j.input;
        while(true){
            bool eof=false;
            {std::lock_guard<std::mutex> lock(mutex);
                if(sent){queued.erase(0,sent);sent=0;}
                queued+=pendingInput;pendingInput.clear();eof=requestedEOF;
            }
            if(queued.empty()&&eof)closefd(input[1]);
            auto now=Clock::now();auto elapsed=std::chrono::duration_cast<std::chrono::milliseconds>(now-begin).count();
            if(!signalled&&(elapsed>=j.timeout||cancelled.load())){
                timed=elapsed>=j.timeout;wasCancelled=cancelled.load();signalled=true;stopAt=now;
                // The unreaped direct child owns this freshly created process group.
                if(waited==0&&getpgid(pid)==pid)kill(-pid,SIGTERM);closefd(input[1]);
            }
            if(signalled&&waited==0&&now-stopAt>std::chrono::milliseconds(500)&&getpgid(pid)==pid)kill(-pid,SIGKILL);
            struct pollfd p[]={{out[0],POLLIN,0},{err[0],POLLIN,0},{queued.empty()?-1:input[1],POLLOUT,0}};
            int n=poll(p,3,20);if(n<0&&errno!=EINTR){limited=true;break;}
            for(int i=0;i<2;i++)if(p[i].fd>=0&&p[i].revents){
                char b[8192];ssize_t count=read(p[i].fd,b,sizeof(b));
                if(count>0){auto &s=i==0?output:error;size_t keep=std::min(static_cast<size_t>(count),cap-s.size());s.append(b,keep);if(keep<static_cast<size_t>(count))limited=true;}
                else if(count==0){if(i==0){outEOF=true;closefd(out[0]);}else{errEOF=true;closefd(err[0]);}}
                else if(errno!=EINTR&&errno!=EAGAIN){limited=true;if(i==0)closefd(out[0]);else closefd(err[0]);}
            }
            if(input[1]>=0&&p[2].revents){ssize_t nwrite=write(input[1],queued.data(),queued.size());
                if(nwrite>0){sent=static_cast<size_t>(nwrite);std::lock_guard<std::mutex> lock(mutex);bufferedInput-=sent;}
                else if(nwrite<0&&errno!=EINTR&&errno!=EAGAIN)closefd(input[1]);}
            {std::lock_guard<std::mutex> lock(mutex);liveOutput=output;liveError=error;stdinClosed=input[1]<0;}
            if(waited==0){do{waited=waitpid(pid,&status,WNOHANG);}while(waited<0&&errno==EINTR);}
            if(waited!=0&&out[0]<0&&err[0]<0)break;
            if(signalled&&now-stopAt>std::chrono::seconds(3)){limited=true;break;}
        }
        if(waited==0){if(getpgid(pid)==pid)kill(-pid,SIGKILL);do{waited=waitpid(pid,&status,0);}while(waited<0&&errno==EINTR);}
    }
    for(int *p:{input,out,err}){closefd(p[0]);closefd(p[1]);}
    char *resolved=realpath(j.path.c_str(),nullptr);std::string real=resolved?resolved:"";free(resolved);
    const auto ms=std::chrono::duration_cast<std::chrono::milliseconds>(Clock::now()-begin).count();
    std::string actorDomain=read_small("/proc/self/attr/current");
    std::ofstream f(j.root+"/"+j.id+".result.json");
    f<<"{\"schema\":1,\"run_id\":"<<quote(j.id)<<",\"complete\":true,\"uid\":"<<getuid()<<",\"pid\":"<<getpid()
      <<",\"domain\":"<<quote(actorDomain)<<",\"entry\":"<<quote(j.path)<<",\"resolved_entry\":"<<quote(real)
      <<",\"child_pid\":"<<pid<<",\"child_domain\":"<<quote(childDomain)<<",\"child_status\":"<<quote(childStatus)
      <<",\"spawn_error\":"<<rc<<",\"waited_pid\":"<<waited<<",\"raw_status\":"<<status
      <<",\"exit_code\":"<<(waited==pid&&WIFEXITED(status)?WEXITSTATUS(status):-1)
      <<",\"signal\":"<<(waited==pid&&WIFSIGNALED(status)?WTERMSIG(status):0)
      <<",\"timed_out\":"<<(timed?"true":"false")<<",\"cancelled\":"<<(wasCancelled?"true":"false")
      <<",\"output_or_io_limited\":"<<(limited?"true":"false")<<",\"stdout_eof\":"<<(outEOF?"true":"false")
      <<",\"stderr_eof\":"<<(errEOF?"true":"false")<<",\"elapsed_ms\":"<<ms
      <<",\"stdout_base64\":"<<quote(b64(output))<<",\"stderr_base64\":"<<quote(b64(error))<<"}\n";
    f.close();unlink((j.root+"/"+j.id+".running").c_str());
    std::lock_guard<std::mutex> lock(mutex);stdinClosed=true;pendingInput.clear();active.clear();
}
napi_value run(napi_env env,napi_callback_info info){
    size_t count=6;napi_value args[6];napi_get_cb_info(env,info,&count,args,nullptr,nullptr);
    if(count!=5&&count!=6)return reply(env,"Rejected: expected five or six arguments");
    Job j;j.root=str(env,args[0],1600);j.id=str(env,args[1],64);j.input=str(env,args[3]);
    if(count==6&&napi_get_value_bool(env,args[5],&j.interactive)!=napi_ok)return reply(env,"Rejected: interactive flag");
    size_t inputSize=0;if(napi_get_value_string_utf8(env,args[3],nullptr,0,&inputSize)!=napi_ok||inputSize>65536)return reply(env,"Rejected: stdin size");
    if(j.root.rfind("/data/storage/",0)!=0||j.root.find("..")!=std::string::npos||!id_ok(j.id))return reply(env,"Rejected: directory/run ID");
    if(napi_get_value_int32(env,args[4],&j.timeout)!=napi_ok||j.timeout<100||j.timeout>120000)return reply(env,"Rejected: timeout");
    bool array=false;uint32_t length=0;napi_is_array(env,args[2],&array);napi_get_array_length(env,args[2],&length);
    if(!array||length<1||length>32)return reply(env,"Rejected: argv length");
    for(uint32_t i=0;i<length;i++){napi_value v;napi_get_element(env,args[2],i,&v);size_t size=0;
        if(napi_get_value_string_utf8(env,v,nullptr,0,&size)!=napi_ok||size>8192)return reply(env,"Rejected: argument");
        auto s=str(env,v,8192);if(s.find('\0')!=std::string::npos)return reply(env,"Rejected: NUL argument");j.args.push_back(s);}
    if(j.args[0].empty())return reply(env,"Rejected: empty executable");
    if(j.args[0].find('/')!=std::string::npos){
        if(j.args[0][0]!='/')return reply(env,"Rejected: use an absolute executable path");
        j.path=j.args[0];
    }else{
        // Public HNP first, then the app's inherited system PATH. No shell.
        std::string paths="/data/service/hnp/bin:";
        if(const char *path=getenv("PATH"))paths+=path;
        paths+=":/system/bin";
        size_t start=0;
        while(start<paths.size()){
            size_t end=paths.find(':',start);if(end==std::string::npos)end=paths.size();
            std::string dir=paths.substr(start,end-start);
            if(!dir.empty()&&dir[0]=='/'&&access((dir+"/"+j.args[0]).c_str(),X_OK)==0){j.path=dir+"/"+j.args[0];break;}
            start=end+1;
        }
        if(j.path.empty())return reply(env,"Rejected: executable not found in public HNP or PATH");
    }
    std::lock_guard<std::mutex> lock(mutex);if(!active.empty())return reply(env,"Rejected: busy");
    if(access((j.root+"/"+j.id+".result.json").c_str(),F_OK)==0)return reply(env,"Rejected: duplicate run ID");
    int fd=open((j.root+"/"+j.id+".running").c_str(),O_WRONLY|O_CREAT|O_EXCL|O_CLOEXEC,0600);
    if(fd<0)return reply(env,"Rejected: cannot reserve run ID");close(fd);active=j.id;cancelled=false;
    pendingInput.clear();liveOutput.clear();liveError.clear();requestedEOF=!j.interactive;stdinClosed=false;bufferedInput=j.input.size();
    const auto reservation=j.root+"/"+j.id+".running";
    try{std::thread(work,std::move(j)).detach();}catch(...){unlink(reservation.c_str());active.clear();return reply(env,"Rejected: worker creation failed");}
    return reply(env,"Accepted");
}
napi_value cancel(napi_env env,napi_callback_info info){size_t n=1;napi_value a;napi_get_cb_info(env,info,&n,&a,nullptr,nullptr);
    std::lock_guard<std::mutex> lock(mutex);if(n!=1||str(env,a,64)!=active||active.empty())return reply(env,"Rejected: no matching active run");cancelled=true;return reply(env,"Cancel requested");}
napi_value sendInput(napi_env env,napi_callback_info info){
    size_t n=3;napi_value a[3];napi_get_cb_info(env,info,&n,a,nullptr,nullptr);
    if(n!=3)return reply(env,"Rejected: expected ID, text, EOF");
    size_t bytes=0;bool eof=false;
    if(napi_get_value_string_utf8(env,a[1],nullptr,0,&bytes)!=napi_ok||bytes>65536||napi_get_value_bool(env,a[2],&eof)!=napi_ok)return reply(env,"Rejected: input size/type");
    std::lock_guard<std::mutex> lock(mutex);
    if(active.empty()||str(env,a[0],64)!=active||stdinClosed||requestedEOF||cancelled.load())return reply(env,"Rejected: stdin closed or no matching active run");
    if(bufferedInput+bytes>65536)return reply(env,"Rejected: stdin queue full; retry later");
    pendingInput+=str(env,a[1]);bufferedInput+=bytes;requestedEOF=eof;return reply(env,eof?"EOF queued after pending input":"Input queued");
}
napi_value snapshot(napi_env env,napi_callback_info info){
    size_t n=1;napi_value a;napi_get_cb_info(env,info,&n,&a,nullptr,nullptr);
    std::lock_guard<std::mutex> lock(mutex);
    if(n!=1||active.empty()||str(env,a,64)!=active)return reply(env,"{}");
    return reply(env,"{\"stdout_base64\":"+quote(b64(liveOutput))+",\"stderr_base64\":"+quote(b64(liveError))+",\"stdin_closed\":"+(stdinClosed||requestedEOF?"true":"false")+"}");
}
napi_value init(napi_env env,napi_value ex){napi_property_descriptor d[]={{"run",nullptr,run,nullptr,nullptr,nullptr,napi_default,nullptr},{"cancel",nullptr,cancel,nullptr,nullptr,nullptr,napi_default,nullptr},{"sendInput",nullptr,sendInput,nullptr,nullptr,nullptr,napi_default,nullptr},{"snapshot",nullptr,snapshot,nullptr,nullptr,nullptr,napi_default,nullptr}};napi_define_properties(env,ex,4,d);return ex;}
napi_module module={1,0,nullptr,init,"clienthnp",nullptr,{0}};
__attribute__((constructor))void reg(){napi_module_register(&module);}
}
