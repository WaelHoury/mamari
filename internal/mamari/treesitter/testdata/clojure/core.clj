(ns clj-test.core)

(defn find-user-clj [id]
  (helper-clj id))

(defn helper-clj [x]
  (let [y x]
    y))

(defn load-user-clj [repo id]
  (if (nil? id)
    nil
    (find-user-clj id)))
