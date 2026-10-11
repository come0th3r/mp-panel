import { useEffect, useState } from 'react';
import { Button, Checkbox } from '@radix-ui/themes';
import { api, enc } from './api';
import type { User } from './api';
import { Empty, Loading, Notice, Unavailable, useAction, useResource, useUnsavedChanges } from './ui';

type Assignments = { user_ids: string[] };

export function ServerAccess({ id }: { id: string }) {
  const path = `/servers/${enc(id)}/operators`;
  const users = useResource<User[]>('/users');
  const assignments = useResource<Assignments>(path);
  const [selected, setSelected] = useState<string[]>([]);
  const action = useAction();
  useEffect(() => { if (assignments.data) setSelected(assignments.data.user_ids); }, [assignments.data]);
  const dirty = !!assignments.data && JSON.stringify([...selected].sort()) !== JSON.stringify([...assignments.data.user_ids].sort());
  useUnsavedChanges(dirty);
  const operators = users.data?.filter(user => user.role === 'operator') || [];
  const retry = () => { users.refresh(); assignments.refresh(); };
  return <section className="narrow-form">
    <h2>Операторы сервера</h2>
    <p className="prose">Выбранные операторы получают полный доступ к этому серверу: консоль, файлы, настройки, смена ядра, копии, сеть и удаление. Назначать доступ может только владелец панели.</p>
    <Notice error={users.error || assignments.error || action.error} success={action.success} />
    {users.loading || assignments.loading ? <Loading /> : users.data === undefined || assignments.data === undefined ? <Unavailable onRetry={retry} /> : operators.length ? <form onSubmit={e => {
      e.preventDefault();
      void action.run(async () => {
        await api(path, 'PUT', { user_ids: selected });
        assignments.refresh();
      }, 'Доступ операторов сохранен.');
    }}>
      <fieldset className="operator-list" disabled={action.busy}>
        <legend>Кому разрешено управление</legend>
        {operators.map(operator => <label key={operator.id} className="check-label">
          <Checkbox checked={selected.includes(operator.id)} onCheckedChange={checked => setSelected(current => checked === true ? [...current, operator.id] : current.filter(userID => userID !== operator.id))} />
          <span>{operator.username}</span>
        </label>)}
      </fieldset>
      <small className="subline">Снимите отметку, чтобы отозвать доступ. Администраторы и владелец имеют доступ ко всем серверам.</small>
      <div className="actions form-actions"><Button type="submit" disabled={!dirty} loading={action.busy}>Сохранить доступ</Button></div>
    </form> : <Empty title="Операторов пока нет" action={<Button asChild variant="soft"><a href="#/settings">Открыть пользователей</a></Button>}>Создайте пользователя с ролью «Оператор» в настройках панели. Затем назначьте ему этот сервер здесь.</Empty>}
  </section>;
}
