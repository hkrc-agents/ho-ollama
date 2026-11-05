/* Deterministic controls for the automation harness, not a replacement inference CLI. */
#include <stdio.h>
#include <string.h>
#include <unistd.h>
static int advance(void) {
    char c;
    while(read(0,&c,1)==1){if(c=='\n')return 0;}
    return 68;
}
int main(int argc,char **argv) {
    if(argc!=2)return 64;
    if(!strcmp(argv[1],"io")) {
        char buffer[4096];ssize_t n;
        while((n=read(0,buffer,sizeof(buffer)))>0){ssize_t at=0;while(at<n){ssize_t w=write(1,buffer+at,(size_t)(n-at));if(w<=0)return 65;at+=w;}}
        fputs("fixture-stderr\n",stderr);return n<0?66:7;
    }
    if(!strcmp(argv[1],"wait")){fputs("fixture-waiting\n",stderr);fflush(stderr);for(;;)pause();}
    if(!strcmp(argv[1],"binary")){const unsigned char b[]={0,255,128,10};return write(1,b,sizeof(b))==sizeof(b)?0:67;}
    if(!strcmp(argv[1],"utf8")) {
        /* Deliberately split Chinese, emoji and CSI across cumulative snapshots. */
        if(write(1,"\xe4",1)!=1||write(2,"\033[",2)!=2)return 67;
        if(advance())return 68;
        if(write(1,"\xbd\xa0\xe5\xa5",4)!=4)return 67;
        const char mode[]="?25l\033[1G\xe8";
        if(write(2,mode,sizeof(mode)-1)!=(ssize_t)sizeof(mode)-1)return 67;
        if(advance())return 68;
        if(write(1,"\xbd\xf0\x9f",3)!=3||write(2,"\xad\xa6\xe5\x91",4)!=4)return 67;
        if(advance())return 68;
        if(write(1,"\x98\x80\n",3)!=3||write(2,"\x8a\xf0\x9f",3)!=3)return 67;
        if(advance())return 68;
        return write(2,"\x98\x80\n\033[?25h",9)==9?0:67;
    }
    return 64;
}
