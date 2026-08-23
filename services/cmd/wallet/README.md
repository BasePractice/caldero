# Кошелек

## Функциональные требования

1. Хранить баланс внутренней валюты системы;
2. Выполнять операции по списанию и начислению валюты;
3. Выполнять операции перевода валюты с одного кошелька на другой;
4. Обеспечивать транзакционность операций. Работать в синхронном и асинхронном режиме;
5. Хранение истории операций (транзакций);
6. Партиционирование таблиц транзакций по критериям: время создание транзакции и разделением за один месяц (реализовано: `PARTITION BY RANGE (created_at)`, окно партиций создаётся миграцией, автоматическое продление — T-067);
7. Поддерживать разные типы кошельков, а именно: пользовательский и общий;
8. Разные типы кошельков, пока только один (PLAIN). У пользователя может быть только один кошелек одного типа;
9. Статусная модель кошелька: ACTIVE, BLOCKED, DELETED, CLOSED. Из состояния CLOSE, BLOCKED можно перейти в состояние
   ACTIVE, но состояние DELETE является терминальным;
10. Если у сервиса запрашивается кошелек для пользователя и его не существует, он создается автоматически с типом PLAIN
    и статусом ACTIVE;

### Запросы

Получаем информацию о кошельках пользователя

```sql
WITH wlt AS (SELECT *
             FROM wallet
             WHERE user_id = '4d91fbf0-07a7-4a0d-8b41-19e90f4540c0'
               AND state <> 'DELETED'),
     trans_debit AS (SELECT SUM(t.value) AS value, wlt.id
                     FROM transaction t
                              JOIN wlt ON t.target = wlt.id
                     WHERE t.state = 'RESERVED'
                       AND t.operation = 'DEBIT'
                     GROUP BY wlt.id),
     trans_credit AS (SELECT SUM(t.value) AS value, wlt.id
                      FROM transaction t
                               JOIN wlt ON t.target = wlt.id
                      WHERE t.state = 'RESERVED'
                        AND t.operation = 'CREDIT'
                      GROUP BY wlt.id),
     trans AS (SELECT COUNT(t.id) AS count, wlt.id
               FROM transaction t
                        JOIN wlt ON t.target = wlt.id
               GROUP BY wlt.id)
SELECT wlt.id                          AS id,
       wlt.state                       AS state,
       wlt.type                        AS type,
       wlt.balance                     AS balance,
       COALESCE(trans.count, 0)        AS trans_c,
       COALESCE(trans_debit.value, 0)  AS dbt_value,
       COALESCE(trans_credit.value, 0) AS crd_value
FROM wlt
         LEFT JOIN trans ON wlt.id = trans.id
         LEFT JOIN trans_debit ON wlt.id = trans_debit.id
         LEFT JOIN trans_credit ON wlt.id = trans_credit.id
ORDER BY wlt.created_at;
```
