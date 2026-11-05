/* Compatibility launcher: RLIMIT_AS permanently disabled by user policy. */
#include <stdio.h>
#include <unistd.h>
int main(int argc,char **argv){
    if(argc<3)return 125;
    fprintf(stderr,"PICO_SERVICE_LIMIT pid=%ld RLIMIT_AS=disabled\n",(long)getpid());
    execv(argv[2],argv+2);perror("execv");return 125;
}
