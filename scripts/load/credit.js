// Нагрузочный сценарий для сервиса кредитов.
//
// Запуск: make load-test (поднимает k6 в docker).
// Сценарий намеренно бьёт в два разных эндпоинта: запись и чтение ведут себя
// по-разному, и общее число запросов в секунду без этого разделения
// ничего не говорит.
import http from 'k6/http';
import { check, group } from 'k6';

const baseUrl = __ENV.BASE_URL || 'http://host.docker.internal:51052';
const operator = __ENV.OPERATOR_ID || '0f95e97c-0ea4-476f-9146-d015ec22e240';

export const options = {
    scenarios: {
        // Плавный рост, а не мгновенный всплеск: мгновенный измеряет
        // поведение при перегрузке, а не пропускную способность.
        ramp: {
            executor: 'ramping-vus',
            startVUs: 1,
            stages: [
                { duration: '10s', target: 10 },
                { duration: '20s', target: 10 },
                { duration: '5s', target: 0 },
            ],
        },
    },
    thresholds: {
        // Пороги из реестра требований (NFR-01).
        'http_req_duration{operation:read}': ['p(95)<200'],
        'http_req_duration{operation:write}': ['p(95)<200'],
        'checks': ['rate>0.99'],
    },
};

export default function () {
    const headers = { 'X-Authorized-Id': operator, 'Content-Type': 'application/json' };

    group('write', function () {
        const payload = JSON.stringify({
            user_id: operator,
            type: 'SIMPLE',
            kind: 'ANN',
            rate_bp: 2400,
            balance: 120000000,
            month: 36,
        });
        const response = http.post(`${baseUrl}/credit`, payload, {
            headers: headers,
            tags: { operation: 'write' },
        });
        check(response, { 'кредит создан': (r) => r.status === 201 });
    });

    group('read', function () {
        // Чтение несуществующего кредита: путь до базы и обратно тот же,
        // а состояние между прогонами не накапливается.
        const response = http.get(
            `${baseUrl}/credits/00000000-0000-0000-0000-000000000000/schedule`,
            { headers: headers, tags: { operation: 'read' } },
        );
        check(response, { 'ответ получен': (r) => r.status === 404 });
    });
}
