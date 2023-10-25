FROM jepsen-debian-base:dev1

ENV container=docker
STOPSIGNAL SIGRTMIN+3

ENV LEIN_ROOT true

#
# Jepsen dependencies
#
RUN apt-get -qy update && \
    apt-get -qy install \
        curl dos2unix emacs git gnuplot graphviz htop iputils-ping libjna-java openjdk-17-jdk-headless pssh screen vim wget

RUN wget https://raw.githubusercontent.com/technomancy/leiningen/stable/bin/lein && \
    mv lein /usr/bin && \
    chmod +x /usr/bin/lein && \
    lein self-install

ADD ./bashrc /root/.bashrc
ADD ./init.sh /init.sh
RUN dos2unix /init.sh /root/.bashrc \
    && chmod +x /init.sh

CMD /init.sh
