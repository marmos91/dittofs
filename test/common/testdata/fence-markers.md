# Fence markers

A line that starts with three backticks or three tildes, after any indentation
and with anything after them, opens a fence or closes the open one, whichever
of the two markers opened it.

```
| IN1 | bug | inside a backtick fence | - |
~~~
| OUT1 | bug | a tilde line closes a backtick fence | - |
        ```
| IN2 | bug | a marker indented by eight spaces still opens a fence | - |
```text after the marker
| OUT2 | bug | text after a marker doesn't stop it closing the fence | - |
```inline``` code at the start of a line opens a fence
| IN3 | bug | so this row is inside one | - |
```
| OUT3 | bug | after the fence closes | - |
