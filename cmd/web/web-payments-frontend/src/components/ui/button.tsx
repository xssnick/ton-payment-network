import { ButtonHTMLAttributes } from "react";

export function Button(props: ButtonHTMLAttributes<HTMLButtonElement>) {
    return (
        <button
            {...props}
            className={`px-4 py-2 rounded-xl font-medium transition ${props.className}`}
        />
    );
}