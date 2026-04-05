import type { Task } from '../types';

interface TaskListProps {
  tasks: Task[];
  onRefresh?: () => void;
}

export function TaskList({ tasks, onRefresh }: TaskListProps) {
  const statusColors: Record<string, string> = {
    pending: '#666',
    in_progress: '#2196f3',
    completed: '#4caf50',
  };

  return (
    <div className="task-list">
      <div className="task-list-header">
        <h3>Tasks</h3>
        {onRefresh && (
          <button onClick={onRefresh} className="refresh-button">
            Refresh
          </button>
        )}
      </div>

      {tasks.length === 0 ? (
        <div className="task-list-empty">No tasks</div>
      ) : (
        <ul className="task-items">
          {tasks.map((task) => (
            <li key={task.id} className="task-item">
              <div className="task-status">
                <span
                  className="status-indicator"
                  style={{ backgroundColor: statusColors[task.status] }}
                />
              </div>
              <div className="task-content">
                <div className="task-subject">{task.subject}</div>
                {task.description && (
                  <div className="task-description">{task.description}</div>
                )}
                {task.active_form && (
                  <div className="task-active-form">{task.active_form}</div>
                )}
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
