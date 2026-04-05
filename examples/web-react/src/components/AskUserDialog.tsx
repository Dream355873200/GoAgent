import { useState } from 'react';
import type { AskUserEvent } from '../types';

interface AskUserDialogProps {
  event: AskUserEvent;
  onRespond: (answer: string) => void;
}

export function AskUserDialog({ event, onRespond }: AskUserDialogProps) {
  const [answer, setAnswer] = useState('');

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (answer.trim()) {
      onRespond(answer.trim());
      setAnswer('');
    }
  };

  return (
    <div className="dialog-overlay">
      <div className="dialog ask-user-dialog">
        <h2>Question</h2>

        <form onSubmit={handleSubmit}>
          <div className="question-content">
            <p className="question-text">{event.question}</p>
          </div>

          <div className="answer-section">
            <label htmlFor="answer-input">Your Answer:</label>
            <textarea
              id="answer-input"
              value={answer}
              onChange={(e) => setAnswer(e.target.value)}
              placeholder="Type your answer here..."
              className="answer-input"
              rows={4}
              autoFocus
            />
          </div>

          <div className="dialog-actions">
            <button
              type="submit"
              className="btn btn-primary"
              disabled={!answer.trim()}
            >
              Submit
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
