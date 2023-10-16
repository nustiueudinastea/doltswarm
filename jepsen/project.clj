(defproject jepsen.doltswarm "0.1.0-SNAPSHOT"
  :description "Jepsen test for doltswarm"
  :url "http://example.com/FIXME"
  :license {:name "EPL-2.0 OR GPL-2.0-or-later WITH Classpath-exception-2.0"
            :url "https://www.eclipse.org/legal/epl-2.0/"}
  :main jepsen.doltswarm
  :dependencies [[org.clojure/clojure "1.11.1"]
                 [jepsen "0.3.3"]]
  :repl-options {:init-ns jepsen.doltswarm})
