// Единый документ проекта. Собирается командой `make docs`.
//
// Здесь только вёрстка и порядок глав. Текст, объясняющий решения, лежит
// в chapters/, а всё, что выводится из кода — списки эндпоинтов,
// переменных, требований и сервисов — собирается генератором в generated/.
// Так документ не расходится с системой: расхождение роняет сборку.

#set document(title: "Котелок: устройство системы", author: "Проект Котелок")
#set page(
  paper: "a4",
  margin: (top: 2.5cm, bottom: 2.5cm, left: 2.5cm, right: 2cm),
  numbering: "1",
  number-align: center,
)
// Шрифт задан явно: без этого документ собирается по-разному на разных
// машинах, а с кириллицей это заметно сразу.
#set text(font: "New Computer Modern", size: 10.5pt, lang: "ru")
#set par(justify: true, leading: 0.65em)
#set heading(numbering: "1.1")

#show heading.where(level: 1): it => {
  pagebreak(weak: true)
  block(above: 1.2em, below: 0.8em)[#text(size: 16pt, weight: "bold")[#it]]
}
#show heading.where(level: 2): it => block(above: 1em, below: 0.6em)[
  #text(size: 12.5pt, weight: "bold")[#it]
]
#show table: set text(size: 9pt)
#show raw: set text(font: "DejaVu Sans Mono", size: 9pt)
#show link: set text(fill: rgb("#1a4f8a"))

#align(center + horizon)[
  #text(size: 26pt, weight: "bold")[Котелок]

  #v(0.5em)
  #text(size: 14pt)[Устройство системы: функциональность, API, сервисы, развёртывание]

  #v(2em)
  #text(size: 10pt)[Документ собран из исходников проекта.\
  Списки требований, эндпоинтов, переменных и сервисов сгенерированы,\
  а не переписаны руками.]
]

#pagebreak()
#outline(depth: 2, indent: auto)

#include "chapters/overview.typ"
#include "chapters/architecture.typ"
#include "generated/services.typ"
#include "generated/endpoints.typ"
#include "chapters/deployment.typ"
#include "generated/env.typ"
#include "generated/requirements.typ"
#include "chapters/security.typ"
#include "generated/adr.typ"
